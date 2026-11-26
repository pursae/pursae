package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/i18n"
	"wpgo/internal/mail"
	"wpgo/internal/store"
)

// Pursae Forum v0.2.5 engagement: likes, topic views, accepted answers, who is online, the points
// ledger, topic subscriptions (new reply emails), post reports and new-topic drafts.
//
//   forum_likes          (post_id, user_id)            one like per member and post
//   forum_topic_views    (topic_id, views)             the view beacon's counter
//   forum_points         ledger rows: topic|reply|solution|like|manual (members of the week)
//   forum_subscriptions  (topic_id, user_id)           "email me when someone replies"
//   forum_reports        open|resolved reports         the moderation queue's Reported view
//
// Cache rules: nothing visitor-specific is rendered into a cacheable page. Views are counted by a
// beacon (POST forum/v1/topics/<id>/view) so a page served from Pursae Cache still counts; who is
// online is refreshed by POST forum/v1/presence. Likes, solutions, subscriptions and reports are
// member actions: member pages are no-store and the forms carry the forum nonce.

// Ledger ------------------------------------------------------------------------------------------

// forumLedger records a point-earning event (once: kind, post, actor). Points come from the levels
// setting; a zero amount or an anonymous author records nothing. Errors are logged, never returned:
// the ledger dates points, the totals are computed from the posts.
func (s *Server) forumLedger(ctx context.Context, userID int64, kind string, postID, actorID int64, at time.Time) {
	if userID <= 0 {
		return
	}
	lv := s.forumLevelsLoad(ctx)
	pts := 0
	switch kind {
	case "topic":
		pts = lv.TopicPoints
	case "reply":
		pts = lv.ReplyPoints
	case "solution":
		pts = lv.SolutionPoints
	case "like":
		pts = lv.LikePoints
	}
	if pts == 0 {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := s.store.Pool.Exec(context.WithoutCancel(ctx), `INSERT INTO forum_points (user_id, kind, points, post_id, actor_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (kind, post_id, coalesce(actor_id, 0)) WHERE kind <> 'manual' DO NOTHING`,
		userID, kind, pts, forumNullID(postID), forumNullID(actorID), at); err != nil {
		log.Printf("forum: points ledger: %v", err)
	}
}

// forumLedgerDrop removes the ledger row of an undone event (an unlike, a withdrawn solution).
func (s *Server) forumLedgerDrop(ctx context.Context, kind string, postID, actorID int64) {
	if _, err := s.store.Pool.Exec(context.WithoutCancel(ctx), `DELETE FROM forum_points WHERE kind = $1 AND post_id = $2
		AND coalesce(actor_id, 0) = $3`, kind, postID, actorID); err != nil {
		log.Printf("forum: points ledger: %v", err)
	}
}

// forumAdjustPoints adds a manual adjustment (positive or negative) to a member's points
// (administrators: the member screen).
func (s *Server) forumAdjustPoints(ctx context.Context, userID, actorID int64, points int, note string) error {
	if points == 0 || points < -100000 || points > 100000 {
		return errors.New("Please enter a number of points between -100000 and 100000.")
	}
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO forum_points (user_id, kind, points, actor_id, note) VALUES ($1, 'manual', $2, $3, $4)`,
		userID, points, forumNullID(actorID), forumPlainLine(note, 200)); err != nil {
		return err
	}
	s.forumPurge(ctx)
	return nil
}

// forumSQLExtraStats groups, by member, accepted answers (not on their own topics), likes received
// (not their own) and manual point adjustments: what the v0.2.5 points add to topics and replies.
const forumSQLExtraStats = `SELECT x.uid AS user_id, sum(x.sol)::int AS solutions, sum(x.lk)::int AS likes, sum(x.bonus)::int AS bonus FROM (
	SELECT r.author_id AS uid, 1 AS sol, 0 AS lk, 0 AS bonus FROM posts t JOIN posts r ON r.type = 'reply' AND r.parent_id = t.id
		AND r.id = (t.meta->>'` + forumMetaSolution + `')::bigint
		WHERE t.type = 'topic' AND t.meta ? '` + forumMetaSolution + `' AND t.status = 'publish' AND coalesce(t.meta->>'` + forumMetaSpam + `','') <> 'true'
		AND r.status = 'publish' AND coalesce(r.meta->>'` + forumMetaSpam + `','') <> 'true' AND r.author_id IS DISTINCT FROM t.author_id
	UNION ALL
	SELECT p.author_id, 0, 1, 0 FROM forum_likes l JOIN posts p ON p.id = l.post_id
		WHERE p.status = 'publish' AND coalesce(p.meta->>'` + forumMetaSpam + `','') <> 'true' AND l.user_id IS DISTINCT FROM p.author_id
	UNION ALL
	SELECT fp.user_id, 0, 0, fp.points FROM forum_points fp WHERE fp.kind = 'manual'
) x WHERE x.uid IS NOT NULL`

// forumExtraStats loads solutions, likes received, manual points and the longest posting streak of
// the members (one query each).
func (s *Server) forumExtraStats(ctx context.Context, ids []int64) (map[int64][4]int, error) {
	out := map[int64][4]int{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.store.Pool.Query(ctx, forumSQLExtraStats+` AND x.uid = ANY($1) GROUP BY x.uid`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var v [4]int
		if err := rows.Scan(&id, &v[0], &v[1], &v[2]); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The longest run of consecutive (UTC) days with a published topic or reply.
	rows, err = s.store.Pool.Query(ctx, `SELECT author_id, max(n)::int FROM (
			SELECT author_id, count(*) AS n FROM (
				SELECT author_id, d - (row_number() OVER (PARTITION BY author_id ORDER BY d))::int AS grp FROM (
					SELECT DISTINCT p.author_id, (p.created_at AT TIME ZONE 'UTC')::date AS d FROM posts p
					WHERE p.type IN ('topic','reply') AND p.status = 'publish' AND `+forumNotSpam("p")+` AND p.author_id = ANY($1)) a) b
			GROUP BY author_id, grp) c GROUP BY author_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		v := out[id]
		v[3] = n
		out[id] = v
	}
	return out, rows.Err()
}

// Likes -------------------------------------------------------------------------------------------

// forumLikeLimit bounds a member's likes and unlikes: 60 per 10 minutes.
const forumLikeLimit = 60

var errForumOwnPost = errors.New("forum: you cannot like your own post")

// forumLikeTarget is a likeable post: a published, non-spam topic or reply of a visible topic. It
// returns the post's author and topic.
func (s *Server) forumLikeTarget(ctx context.Context, postID int64) (authorID, topicID int64, err error) {
	var typ string
	var parent int64
	err = s.store.Pool.QueryRow(ctx, `SELECT p.type, coalesce(p.author_id,0), coalesce(p.parent_id,0) FROM posts p WHERE p.id = $1
		AND p.type IN ('topic','reply') AND p.status = 'publish' AND `+forumNotSpam("p")+` AND `+forumSQLLiveParent+` AND `+forumSQLInPublishedForum, postID).Scan(&typ, &authorID, &parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, errForumNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	topicID = postID
	if typ == "reply" {
		topicID = parent
	}
	return authorID, topicID, nil
}

// forumSetLike likes (or unlikes) a post for a member and returns the new state and count. A member
// cannot like their own post; liking twice is a no-op. The post author's ledger follows.
func (s *Server) forumSetLike(ctx context.Context, u *store.User, postID int64, like bool) (bool, int, error) {
	author, topicID, err := s.forumLikeTarget(ctx, postID)
	if err != nil {
		return false, 0, err
	}
	if like && author == u.ID {
		return false, 0, errForumOwnPost
	}
	if like {
		tag, err := s.store.Pool.Exec(ctx, `INSERT INTO forum_likes (post_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, postID, u.ID)
		if err != nil {
			return false, 0, err
		}
		if tag.RowsAffected() > 0 {
			s.forumLedger(ctx, author, "like", postID, u.ID, time.Now())
		}
	} else {
		tag, err := s.store.Pool.Exec(ctx, `DELETE FROM forum_likes WHERE post_id = $1 AND user_id = $2`, postID, u.ID)
		if err != nil {
			return false, 0, err
		}
		if tag.RowsAffected() > 0 {
			s.forumLedgerDrop(ctx, "like", postID, u.ID)
		}
	}
	n := s.forumLikeCount(ctx, postID)
	s.forumPurgeTopic(ctx, topicID)
	return like, n, nil
}

// forumLikeCount is a post's likes (the demo seed included).
func (s *Server) forumLikeCount(ctx context.Context, postID int64) int {
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM forum_likes WHERE post_id = $1)
		+ coalesce((SELECT (meta->>'`+forumMetaLikesSeed+`')::int FROM posts WHERE id = $1 AND meta ? '`+forumMetaLikesSeed+`'), 0)`, postID).Scan(&n)
	return n
}

// forumLikeCounts are the likes of many posts (the demo seed included).
func (s *Server) forumLikeCounts(ctx context.Context, ids []int64) map[int64]int {
	out := map[int64]int{}
	if len(ids) == 0 {
		return out
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT p.id, (SELECT count(*) FROM forum_likes l WHERE l.post_id = p.id)
		+ coalesce((p.meta->>'`+forumMetaLikesSeed+`')::int, 0) FROM posts p WHERE p.id = ANY($1)`, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if rows.Scan(&id, &n) == nil {
			out[id] = n
		}
	}
	return out
}

// forumLikedBy are the posts among ids the member likes.
func (s *Server) forumLikedBy(ctx context.Context, userID int64, ids []int64) map[int64]bool {
	out := map[int64]bool{}
	if userID <= 0 || len(ids) == 0 {
		return out
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT post_id FROM forum_likes WHERE user_id = $1 AND post_id = ANY($2)`, userID, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// forumPurgeTopic drops the cached pages of one topic (a like changes nothing else).
func (s *Server) forumPurgeTopic(ctx context.Context, topicID int64) {
	_ = ctx
	s.purgeTags(forumTopicCacheTag(topicID))
}

// forumTopicCacheTag is the cache tag of a topic's pages.
func forumTopicCacheTag(topicID int64) string { return "forum-topic:" + strconv.FormatInt(topicID, 10) }

// Views -------------------------------------------------------------------------------------------

// forumViewSeen remembers which visitor counted which topic (30 minutes), process-local and bounded.
var forumViewSeen = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

const forumViewWindow = 30 * time.Minute

// forumBotRe-like substrings: user agents that never count as a view.
var forumBotAgents = []string{"bot", "crawl", "spider", "slurp", "preview", "headless", "curl", "wget", "python", "httpclient",
	"go-http-client", "monitor", "lighthouse", "facebookexternalhit", "embedly", "scrapy", "phantomjs", "java/"}

// forumIsBot reports whether a user agent belongs to a robot (or is missing).
func forumIsBot(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	if ua == "" {
		return true
	}
	for _, b := range forumBotAgents {
		if strings.Contains(ua, b) {
			return true
		}
	}
	return false
}

// forumVisitorKey identifies a visitor without storing the address: the member id, or a hash of the
// address and the user agent.
func (s *Server) forumVisitorKey(c *gin.Context) (string, bool) {
	if u := currentUser(c); u != nil {
		return "u:" + strconv.FormatInt(u.ID, 10), true
	}
	sum := sha256.Sum256([]byte(s.siteSecret() + "|" + clientIP(c) + "|" + c.Request.UserAgent()))
	return "v:" + hex.EncodeToString(sum[:10]), false
}

// forumCountView counts a view of a published topic by this visitor: bots, repeats within
// forumViewWindow and floods (120 per 10 minutes per network) are not counted. It returns the
// topic's views and whether this one counted.
func (s *Server) forumCountView(c *gin.Context, topicID int64) (int, bool, error) {
	ctx := c.Request.Context()
	var views int
	err := s.store.Pool.QueryRow(ctx, `SELECT coalesce((SELECT views FROM forum_topic_views WHERE topic_id = p.id), 0) FROM posts p
		WHERE p.id = $1 AND p.type = 'topic' AND p.status = 'publish' AND `+forumNotSpam("p")+` AND `+forumPublishedForumsSQL("p"), topicID).Scan(&views)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, errForumNotFound
	}
	if err != nil {
		return 0, false, err
	}
	if forumIsBot(c.Request.UserAgent()) || !forumAllow("view:"+gooikeTruncateIP(clientIP(c)), 120, 10*time.Minute) {
		return views, false, nil
	}
	key, _ := s.forumVisitorKey(c)
	key += "|" + strconv.FormatInt(topicID, 10)
	now := time.Now()
	forumViewSeen.Lock()
	if len(forumViewSeen.m) > 50000 {
		for k, t := range forumViewSeen.m {
			if now.Sub(t) > forumViewWindow {
				delete(forumViewSeen.m, k)
			}
		}
	}
	if t, ok := forumViewSeen.m[key]; ok && now.Sub(t) < forumViewWindow {
		forumViewSeen.Unlock()
		return views, false, nil
	}
	forumViewSeen.m[key] = now
	forumViewSeen.Unlock()
	if err := s.store.Pool.QueryRow(context.WithoutCancel(ctx), `INSERT INTO forum_topic_views (topic_id, views) VALUES ($1, 1)
		ON CONFLICT (topic_id) DO UPDATE SET views = forum_topic_views.views + 1 RETURNING views`, topicID).Scan(&views); err != nil {
		return views, false, err
	}
	return views, true, nil
}

// forumViewSeenReset forgets the counted views (tests).
func forumViewSeenReset() {
	forumViewSeen.Lock()
	clear(forumViewSeen.m)
	forumViewSeen.Unlock()
}

// Online ------------------------------------------------------------------------------------------

// forumPresence is who was seen where, process-local: visitor key → forum (0 = elsewhere) and time.
var forumPresence = struct {
	sync.Mutex
	m map[string]forumSeen
}{m: map[string]forumSeen{}}

type forumSeen struct {
	forum  int64
	at     time.Time
	member bool
}

// forumMarkSeen records that this visitor is on the forum (on forumID's pages when > 0). Robots do
// not count.
func (s *Server) forumMarkSeen(c *gin.Context, forumID int64) {
	if c == nil || c.Request == nil || forumIsBot(c.Request.UserAgent()) {
		return
	}
	key, member := s.forumVisitorKey(c)
	now := time.Now()
	forumPresence.Lock()
	defer forumPresence.Unlock()
	if len(forumPresence.m) > 50000 {
		for k, v := range forumPresence.m {
			if now.Sub(v.at) > time.Hour {
				delete(forumPresence.m, k)
			}
		}
	}
	forumPresence.m[key] = forumSeen{forum: forumID, at: now, member: member}
}

// forumOnlineCount is how many were seen in the online window: on forumID (0: anywhere) and how
// many of them are members.
func (s *Server) forumOnlineCount(forumID int64) (all, members int) {
	window := time.Duration(s.forumLoad().OnlineMinutes) * time.Minute
	now := time.Now()
	forumPresence.Lock()
	defer forumPresence.Unlock()
	for _, v := range forumPresence.m {
		if now.Sub(v.at) > window || (forumID > 0 && v.forum != forumID) {
			continue
		}
		all++
		if v.member {
			members++
		}
	}
	return all, members
}

// forumPresenceReset forgets everyone (tests).
func forumPresenceReset() {
	forumPresence.Lock()
	clear(forumPresence.m)
	forumPresence.Unlock()
}

// Solutions ---------------------------------------------------------------------------------------

var errForumSolveDenied = errors.New("forum: only the topic's author or a moderator can choose the solution")

// forumCanSolve reports whether u may accept an answer in topic t: its author (who may post) or a
// moderator.
func forumCanSolve(u *store.User, st forumSettings, t *forumTopic) bool {
	if u == nil || t == nil || t.Status != "publish" || t.Spam {
		return false
	}
	if u.Can(capForumModerate, nil) {
		return true
	}
	code, _ := forumPostGate(u, st)
	return code == 0 && t.AuthorID == u.ID && t.AuthorID != 0
}

// forumSolve accepts reply replyID as the solution of its topic (or, with replyID 0, withdraws the
// solution of topicID). The reply's author earns the solution points (not on their own topic).
func (s *Server) forumSolve(ctx context.Context, u *store.User, topicID, replyID int64) error {
	t, err := s.forumTopicByID(ctx, topicID)
	if err != nil {
		return err
	}
	if !forumCanSolve(u, s.forumLoad(), t) {
		return errForumSolveDenied
	}
	if replyID == 0 {
		if t.SolutionID > 0 {
			s.forumDropSolution(ctx, topicID, t.SolutionID)
		}
		return nil
	}
	r, err := s.forumReplyByID(ctx, replyID)
	if err != nil || r.TopicID != topicID || r.Status != "publish" || r.Spam {
		return errForumNotFound
	}
	if t.SolutionID == replyID {
		return nil
	}
	if t.SolutionID > 0 {
		s.forumLedgerDrop(ctx, "solution", t.SolutionID, 0)
	}
	m, _ := json.Marshal(map[string]any{forumMetaSolution: replyID, forumMetaSolvedAt: forumStamp(time.Now()), forumMetaSolvedBy: u.ID})
	if _, err := s.store.Pool.Exec(ctx, `UPDATE posts SET meta = coalesce(meta,'{}') || $2::jsonb WHERE id = $1 AND type = 'topic'`, topicID, m); err != nil {
		return err
	}
	if r.AuthorID != t.AuthorID {
		s.forumLedger(ctx, r.AuthorID, "solution", replyID, 0, time.Now())
	}
	s.forumPurge(ctx)
	return nil
}

// forumDropSolution clears a topic's solution when it is replyID (the reply was withdrawn, trashed or
// spammed).
func (s *Server) forumDropSolution(ctx context.Context, topicID, replyID int64) {
	tag, err := s.store.Pool.Exec(ctx, `UPDATE posts SET meta = meta - '`+forumMetaSolution+`' - '`+forumMetaSolvedAt+`' - '`+forumMetaSolvedBy+`'
		WHERE id = $1 AND type = 'topic' AND meta->>'`+forumMetaSolution+`' = $2`, topicID, strconv.FormatInt(replyID, 10))
	if err != nil {
		log.Printf("forum: drop solution: %v", err)
		return
	}
	if tag.RowsAffected() > 0 {
		s.forumLedgerDrop(ctx, "solution", replyID, 0)
		s.forumPurge(ctx)
	}
}

// Reports -----------------------------------------------------------------------------------------

// forumReportReasons are the reasons a member may give.
var forumReportReasons = map[string]string{"spam": "Spam", "abuse": "Rude or abusive", "off_topic": "Off topic", "other": "Something else"}

// forumReportReasonKeys are the reasons in form order.
var forumReportReasonKeys = []string{"spam", "abuse", "off_topic", "other"}

// forumReport files a member's report of a post (once per member and post; a second report updates
// the reason and reopens it).
func (s *Server) forumReport(ctx context.Context, u *store.User, postID int64, reason, note string) error {
	if _, ok := forumReportReasons[reason]; !ok {
		return errors.New("forum: unknown report reason")
	}
	var exists bool
	if err := s.store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM posts p WHERE p.id = $1 AND p.type IN ('topic','reply')
		AND p.status = 'publish' AND `+forumNotSpam("p")+` AND `+forumSQLLiveParent+` AND `+forumSQLInPublishedForum+`)`, postID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errForumNotFound
	}
	_, err := s.store.Pool.Exec(ctx, `INSERT INTO forum_reports (post_id, user_id, reason, note) VALUES ($1, $2, $3, $4)
		ON CONFLICT (post_id, user_id) WHERE user_id IS NOT NULL DO UPDATE SET reason = EXCLUDED.reason, note = EXCLUDED.note,
			status = 'open', created_at = now(), resolved_by = NULL, resolved_at = NULL`, postID, u.ID, reason, forumPlainLine(note, 500))
	return err
}

// forumResolveReports closes the open reports of posts (a moderator dealt with them).
func (s *Server) forumResolveReports(ctx context.Context, actorID int64, postIDs ...int64) {
	if len(postIDs) == 0 {
		return
	}
	if _, err := s.store.Pool.Exec(context.WithoutCancel(ctx), `UPDATE forum_reports SET status = 'resolved', resolved_by = $2, resolved_at = now()
		WHERE post_id = ANY($1) AND status = 'open'`, postIDs, forumNullID(actorID)); err != nil {
		log.Printf("forum: resolve reports: %v", err)
	}
}

// forumReportSummary is a reported post's open reports.
type forumReportSummary struct {
	Count   int
	Reasons []string // distinct reason keys
	Notes   []string // the members' notes (plain text)
}

// forumOpenReports are the open reports of posts.
func (s *Server) forumOpenReports(ctx context.Context, ids []int64) map[int64]*forumReportSummary {
	out := map[int64]*forumReportSummary{}
	if len(ids) == 0 {
		return out
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT post_id, reason, note FROM forum_reports WHERE post_id = ANY($1) AND status = 'open' ORDER BY created_at`, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var reason, note string
		if rows.Scan(&id, &reason, &note) != nil {
			continue
		}
		r := out[id]
		if r == nil {
			r = &forumReportSummary{}
			out[id] = r
		}
		r.Count++
		dup := false
		for _, x := range r.Reasons {
			dup = dup || x == reason
		}
		if !dup {
			r.Reasons = append(r.Reasons, reason)
		}
		if note != "" && len(r.Notes) < 5 {
			r.Notes = append(r.Notes, note)
		}
	}
	return out
}

// Subscriptions -----------------------------------------------------------------------------------

// forumSubscribe subscribes (or unsubscribes) a member to a topic's replies.
func (s *Server) forumSubscribe(ctx context.Context, userID, topicID int64, on bool) error {
	if on {
		_, err := s.store.Pool.Exec(ctx, `INSERT INTO forum_subscriptions (topic_id, user_id) SELECT $1, $2
			WHERE EXISTS (SELECT 1 FROM posts WHERE id = $1 AND type = 'topic') ON CONFLICT DO NOTHING`, topicID, userID)
		return err
	}
	_, err := s.store.Pool.Exec(ctx, `DELETE FROM forum_subscriptions WHERE topic_id = $1 AND user_id = $2`, topicID, userID)
	return err
}

// forumSubscribed reports whether the member follows the topic.
func (s *Server) forumSubscribed(ctx context.Context, userID, topicID int64) bool {
	var ok bool
	_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM forum_subscriptions WHERE topic_id = $1 AND user_id = $2)`, topicID, userID).Scan(&ok)
	return ok
}

// forumUnsubKey is the key of a topic's unsubscribe link for a member (HMAC of the site secret).
func (s *Server) forumUnsubKey(topicID, userID int64) string {
	mac := hmac.New(sha256.New, []byte(s.siteSecret()+"|forum-unsubscribe"))
	mac.Write([]byte(strconv.FormatInt(topicID, 10) + "|" + strconv.FormatInt(userID, 10)))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// forumUnsubLink is the unsubscribe link of an email.
func (s *Server) forumUnsubLink(ctx context.Context, topicID, userID int64) string {
	return s.forumURL(ctx, "unsubscribe") + "?t=" + strconv.FormatInt(topicID, 10) + "&u=" + strconv.FormatInt(userID, 10) + "&k=" + s.forumUnsubKey(topicID, userID)
}

// forumCheckUnsub verifies an unsubscribe link's key (constant time).
func (s *Server) forumCheckUnsub(topicID, userID int64, key string) bool {
	if topicID <= 0 || userID <= 0 || len(key) != 32 {
		return false
	}
	return hmac.Equal([]byte(key), []byte(s.forumUnsubKey(topicID, userID)))
}

// forumNotifyMax bounds the emails one reply sends.
const forumNotifyMax = 500

// forumNotifyReply emails the topic's subscribers (not the reply's author, not blocked members) that
// a published reply arrived, each with an unsubscribe link. Nothing while notifications are off.
func (s *Server) forumNotifyReply(ctx context.Context, replyID int64) {
	if !s.forumOn() || !s.forumLoad().Notify {
		return
	}
	r, err := s.forumReplyByID(ctx, replyID)
	if err != nil || r.Status != "publish" || r.Spam {
		return
	}
	t, err := s.forumTopicByID(ctx, r.TopicID)
	if err != nil || t.Status != "publish" || t.Spam {
		return
	}
	var live bool // a forum unpublished meanwhile sends nothing (its topics are not visible)
	if err := s.store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM posts p WHERE p.id = $1 AND `+forumPublishedForumsSQL("p")+`)`, t.ID).Scan(&live); err != nil || !live {
		return
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT u.id, u.email, coalesce(nullif(btrim(u.display_name),''), u.login) FROM forum_subscriptions fs
		JOIN users u ON u.id = fs.user_id WHERE fs.topic_id = $1 AND u.id <> $2 AND u.email <> '' AND NOT `+forumSQLBlocked+`
		ORDER BY fs.created_at LIMIT `+strconv.Itoa(forumNotifyMax), t.ID, r.AuthorID)
	if err != nil {
		log.Printf("forum: subscribers: %v", err)
		return
	}
	type sub struct {
		id          int64
		email, name string
	}
	var subs []sub
	for rows.Next() {
		var x sub
		if rows.Scan(&x.id, &x.email, &x.name) == nil {
			subs = append(subs, x)
		}
	}
	rows.Close()
	if len(subs) == 0 {
		return
	}
	site := s.settings.String("blogname")
	if site == "" {
		site = "Pursae"
	}
	cat := s.gooFrontCatalog(ctx)
	tf := func(format string, args ...string) string { return i18n.Sprintf(cat.T(format), args...) }
	who := cat.T("Someone")
	if r.Author != nil {
		who = r.Author.DisplayName
	}
	link := s.forumReplyAnchor(ctx, t, r, nil)
	excerpt := forumExcerpt(r.Content, 60)
	for _, x := range subs {
		body := tf("Hi %s,", x.name) + "\n\n" +
			tf("%1$s replied to “%2$s”:", who, t.Title) + "\n\n" + excerpt + "\n\n" +
			link + "\n\n" +
			cat.T("You get this email because you follow this topic. Stop following it:") + "\n" + s.forumUnsubLink(ctx, t.ID, x.id) + "\n"
		s.sendMail(ctx, mail.Message{Kind: "forum_reply", To: []string{x.email}, Subject: tf("[%1$s] New reply to “%2$s”", site, t.Title), Body: body})
	}
}

// Drafts ------------------------------------------------------------------------------------------

// forumDraftOf is the member's saved new-topic draft (nil when none).
func forumDraftOf(u *store.User) *forumDraft {
	if u == nil || u.Meta == nil || u.Meta[forumUMetaDraft] == nil {
		return nil
	}
	raw, err := json.Marshal(u.Meta[forumUMetaDraft])
	if err != nil {
		return nil
	}
	var d forumDraft
	if json.Unmarshal(raw, &d) != nil || (d.Title == "" && d.Content == "") {
		return nil
	}
	return &d
}

// forumSaveDraft stores (d non-nil) or deletes the member's draft. The content is kept as typed
// (bounded); it is cleaned when posted.
func (s *Server) forumSaveDraft(ctx context.Context, userID int64, d *forumDraft) error {
	if d == nil {
		return s.forumSetUserMeta(ctx, userID, nil, forumUMetaDraft)
	}
	d.Title = forumTruncRunes(forumCleanTitle(d.Title), forumTitleMax)
	d.Content = forumTruncRunes(d.Content, forumContentMax)
	d.Tags = forumTruncRunes(d.Tags, 200)
	d.Saved = time.Now().UTC().Truncate(time.Second)
	return s.forumSetUserMeta(ctx, userID, map[string]any{forumUMetaDraft: d})
}
