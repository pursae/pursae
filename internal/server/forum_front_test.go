package server

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"wpgo/internal/auth"
	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// forumFX is a site with Pursae Forum on: a category "Community" holding the forum "General Chat",
// flood control off (tests post quickly), the admin logged in.
type forumFX struct {
	base  string
	admin *http.Client
	anon  *http.Client
	srv   *Server
	cat   int64
	forum *forumForum
	ctx   context.Context
}

func forumFrontStart(t *testing.T, builder bool) *forumFX {
	t.Helper()
	ts, c, srv := newTestServer(t, builder)
	installAndLogin(t, ts, c)
	ctx := context.Background()
	if err := srv.forumActivate(ctx); err != nil {
		t.Fatal(err)
	}
	fx := &forumFX{base: ts.URL, admin: c, srv: srv, ctx: ctx,
		anon: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	fx.settings(t, func(st *forumSettings) { st.FloodSeconds = 0 })
	cat, err := srv.forumSaveForum(ctx, forumForumInput{Title: "Community", Kind: "category"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := srv.forumSaveForum(ctx, forumForumInput{Title: "General Chat", ParentID: cat, Description: "Talk about anything."}, 1)
	if err != nil {
		t.Fatal(err)
	}
	fx.cat = cat
	if fx.forum, err = srv.forumByID(ctx, fid); err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx *forumFX) settings(t *testing.T, edit func(*forumSettings)) {
	t.Helper()
	st := fx.srv.forumLoad()
	edit(&st)
	if err := fx.srv.forumSave(fx.ctx, st); err != nil {
		t.Fatal(err)
	}
}

// member creates a user with role and returns a logged-in client.
func (fx *forumFX) member(t *testing.T, login, role string) (*http.Client, int64) {
	t.Helper()
	hash, _ := auth.HashPassword("member password 1")
	id, err := fx.srv.store.CreateUser(fx.ctx, store.NewUser{Login: login, Email: login + "@example.test", PasswordHash: hash, Role: role, DisplayName: strings.ToUpper(login[:1]) + login[1:]})
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if code, _, _ := post(t, c, fx.base+"/wp-login", url.Values{"log": {login}, "pwd": {"member password 1"}}); code != http.StatusFound {
		t.Fatalf("login %s: %d", login, code)
	}
	return c, id
}

func (fx *forumFX) url(parts ...string) string { return fx.srv.forumURL(fx.ctx, parts...) }

// forumFormNonce is the _wpnonce of the form posting forum_action=action on the page.
func forumFormNonce(t *testing.T, body, action string) string {
	t.Helper()
	m := regexp.MustCompile(`name="forum_action" value="` + action + `" />\s*<input type="hidden" name="_wpnonce" value="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		m = regexp.MustCompile(`name="_wpnonce" value="([^"]+)" />\s*(?:<input type="hidden" name="forum_action" value="` + action + `")`).FindStringSubmatch(body)
	}
	if m == nil {
		t.Fatalf("no %s form on the page", action)
	}
	return m[1]
}

func forumNoTemplateError(t *testing.T, what, body string) {
	t.Helper()
	for _, bad := range []string{"forum template error", "Template error", "template: ", "[pursae_forum_render]"} {
		if strings.Contains(body, bad) {
			t.Errorf("%s contains %q", what, bad)
		}
	}
}

func getHdr(t *testing.T, c *http.Client, u string) (int, string, http.Header) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, bodyOf(resp), resp.Header
}

// Anonymous views: index, category, forum, topic, profile, search, the reply redirect, canonical
// URLs and slugs, pagination, no forms or nonces on cacheable pages, unpublished forums hidden.
func TestForumFrontViews(t *testing.T) {
	fx := forumFrontStart(t, false)
	srv, ctx := fx.srv, fx.ctx
	fx.settings(t, func(st *forumSettings) { st.TopicsPerPage, st.RepliesPerPage = 5, 5 })

	base := time.Now().Add(-48 * time.Hour)
	var topics []*forumTopic
	for i := 1; i <= 7; i++ {
		tp, err := srv.forumCreateTopic(ctx, forumNewTopic{ForumID: fx.forum.ID, AuthorID: 1, Title: "Topic number " + strconv.Itoa(i),
			Content: "<p>Body of topic " + strconv.Itoa(i) + "</p>", At: base.Add(time.Duration(i) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		topics = append(topics, tp)
	}
	lead := topics[6] // the most recent: first on page 1
	var replies []*forumReply
	for i := 1; i <= 6; i++ {
		r, err := srv.forumCreateReply(ctx, forumNewReply{TopicID: lead.ID, AuthorID: 1, Content: "<p>Answer " + strconv.Itoa(i) + " zebrafish</p>",
			At: base.Add(time.Duration(10+i) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		replies = append(replies, r)
	}

	// Index: the category, the forum, latest topics; cacheable (no nonce, no form, forum cache tag).
	code, body, hdr := getHdr(t, fx.anon, fx.url())
	if code != http.StatusOK {
		t.Fatalf("index: %d", code)
	}
	forumNoTemplateError(t, "index", body)
	wbtfWant(t, "index", body, "Community", "General Chat", "Talk about anything.", "Topic number 7", `class="pf-forum-row`, `class="pf-search"`)
	wbtfBad(t, "index", body, `name="_wpnonce"`, `name="forum_action"`, "wpgo_ct")
	if hdr.Get("Cache-Control") == "no-store" || hdr.Get("X-Wpgo-Cache") == "" {
		t.Errorf("index cache headers: %v", hdr)
	}
	if _, _, hdr := getHdr(t, fx.anon, fx.url()); hdr.Get("X-Wpgo-Cache") != "hit" {
		t.Errorf("index not served from Pursae Cache the second time: %v", hdr.Get("X-Wpgo-Cache"))
	}

	// Category page lists its forums; forum page lists topics with pages; canonical = forum URL.
	if code, body, _ := get(t, fx.anon, fx.url("f", "community")); code != http.StatusOK || !strings.Contains(body, "General Chat") {
		t.Errorf("category page: %d", code)
	}
	code, body, _ = get(t, fx.anon, fx.forum.URL)
	if code != http.StatusOK {
		t.Fatalf("forum: %d", code)
	}
	forumNoTemplateError(t, "forum", body)
	wbtfWant(t, "forum", body, "Topic number 7", "Topic number 3", `rel="canonical" href="`+fx.forum.URL+`"`, `class="pf-pagination"`, "Log in or create an account")
	wbtfBad(t, "forum", body, "Topic number 2", "page_id=0", `name="_wpnonce"`)
	if code, body, _ := get(t, fx.anon, fx.forum.URL+"page/2/"); code != http.StatusOK || !strings.Contains(body, "Topic number 1") || strings.Contains(body, "Topic number 7") {
		t.Errorf("forum page 2: %d", code)
	}
	if code, _, loc := get(t, fx.anon, fx.forum.URL+"page/1/"); code != http.StatusMovedPermanently || loc != forumLocalPath(fx.forum.URL) {
		t.Errorf("page/1: %d %s", code, loc)
	}
	if code, _, _ := get(t, fx.anon, fx.forum.URL+"page/9/"); code != http.StatusNotFound {
		t.Errorf("page beyond the end: %d", code)
	}
	if code, _, _ := get(t, fx.anon, fx.url("f", "nope")); code != http.StatusNotFound {
		t.Errorf("unknown forum: %d", code)
	}

	// Topic: lead post and 5 replies on page 1, the 6th on page 2; wrong / missing slug → 301.
	code, body, _ = get(t, fx.anon, lead.URL)
	if code != http.StatusOK {
		t.Fatalf("topic: %d", code)
	}
	forumNoTemplateError(t, "topic", body)
	wbtfWant(t, "topic", body, "Body of topic 7", "Answer 5 zebrafish", `id="post-`+itoa64(lead.ID)+`"`, `id="post-`+itoa64(replies[0].ID)+`"`,
		`rel="canonical" href="`+lead.URL+`"`, `class="pf-post pf-post--lead`, "General Chat")
	wbtfBad(t, "topic", body, "Answer 6 zebrafish", `name="forum_action"`, `name="_wpnonce"`, "pf-modbar")
	code, body, _ = get(t, fx.anon, srv.forumTopicURL(ctx, lead, 2))
	if code != http.StatusOK || !strings.Contains(body, "Answer 6 zebrafish") || strings.Contains(body, "Body of topic 7") {
		t.Errorf("topic page 2: %d", code)
	}
	if code, _, loc := get(t, fx.anon, fx.url("t", itoa64(lead.ID), "wrong-slug")); code != http.StatusMovedPermanently || loc != forumLocalPath(lead.URL) {
		t.Errorf("wrong slug: %d %s", code, loc)
	}
	if code, _, loc := get(t, fx.anon, fx.url("t", itoa64(lead.ID))); code != http.StatusMovedPermanently || loc != forumLocalPath(lead.URL) {
		t.Errorf("missing slug: %d %s", code, loc)
	}
	if code, _, _ := get(t, fx.anon, fx.url("t", "999999", "x")); code != http.StatusNotFound {
		t.Errorf("unknown topic: %d", code)
	}
	// A non-ASCII title: its slug round-trips (no redirect loop).
	tr, err := srv.forumCreateTopic(ctx, forumNewTopic{ForumID: fx.forum.ID, AuthorID: 1, Title: "Merhaba Dünya çiçeği", Content: "<p>Türkçe</p>"})
	if err != nil {
		t.Fatal(err)
	}
	if code, body, _ := get(t, fx.anon, tr.URL); code != http.StatusOK || !strings.Contains(body, "Merhaba Dünya çiçeği") {
		t.Errorf("unicode topic %s: %d", tr.URL, code)
	}
	// Reply short URL → its page and anchor.
	if code, _, loc := get(t, fx.anon, fx.url("r", itoa64(replies[5].ID))); code != http.StatusFound ||
		loc != forumLocalPath(srv.forumTopicURL(ctx, lead, 2))+"#post-"+itoa64(replies[5].ID) {
		t.Errorf("reply redirect: %d %s", code, loc)
	}

	// Search: private (no-store, noindex), finds replies and topics.
	code, body, hdr = getHdr(t, fx.anon, fx.url("search")+"?q=zebrafish")
	if code != http.StatusOK || hdr.Get("Cache-Control") != "no-store" || hdr.Get("X-Robots-Tag") != "noindex" || !strings.Contains(body, "noindex") {
		t.Errorf("search headers: %d %v", code, hdr)
	}
	forumNoTemplateError(t, "search", body)
	wbtfWant(t, "search", body, "6 results", "Reply to “Topic number 7”", "#post-"+itoa64(replies[5].ID), `class="pf-pagination"`)
	if _, body, _ := get(t, fx.anon, fx.url("search")+"?q=nothinglikethis"); !strings.Contains(body, "Nothing matched") {
		t.Error("empty search result")
	}
	if code, body, _ := get(t, fx.anon, fx.url("search")); code != http.StatusOK || !strings.Contains(body, "Type a few words") {
		t.Errorf("search without query: %d", code)
	}

	// Profile: stats and recent activity.
	code, body, _ = get(t, fx.anon, fx.url("u", "admin"))
	if code != http.StatusOK {
		t.Fatalf("profile: %d", code)
	}
	forumNoTemplateError(t, "profile", body)
	wbtfWant(t, "profile", body, `class="pf-card pf-profile"`, "Recent activity", "Member since", "<dd>8</dd>", "<dd>6</dd>", "Moderator")
	wbtfBad(t, "profile", body, "Block member")
	if code, _, _ := get(t, fx.anon, fx.url("u", "nobody-here")); code != http.StatusNotFound {
		t.Errorf("unknown profile: %d", code)
	}
	// No user enumeration: an account that never posted has no public profile.
	if _, err := srv.store.CreateUser(ctx, store.NewUser{Login: "quiet", Email: "quiet@example.test", PasswordHash: "x", Role: "subscriber"}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := get(t, fx.anon, fx.url("u", "quiet")); code != http.StatusNotFound {
		t.Errorf("profile of a member without posts: %d", code)
	}
	if code, _, _ := get(t, fx.admin, fx.url("u", "quiet")); code != http.StatusOK {
		t.Errorf("profile of a member without posts, as a moderator: %d", code)
	}

	// New topic and edit forms: visitors go to the login form.
	if code, _, loc := get(t, fx.anon, fx.url("f", fx.forum.Slug, "new")); code != http.StatusFound || !strings.Contains(loc, "/forums/login/?redirect=") {
		t.Errorf("new topic as a visitor: %d %s", code, loc)
	}
	if code, _, loc := get(t, fx.anon, fx.url("edit", itoa64(lead.ID))); code != http.StatusFound || !strings.Contains(loc, "/forums/login/") {
		t.Errorf("edit as a visitor: %d %s", code, loc)
	}

	// A forum that is not published: its topics leave the latest list, the search and their URLs.
	hid, err := srv.forumSaveForum(ctx, forumForumInput{Title: "Staff Room", ParentID: fx.cat}, 1)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := srv.forumCreateTopic(ctx, forumNewTopic{ForumID: hid, AuthorID: 1, Title: "Secret plans", Content: "<p>quokka</p>"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE posts SET status = 'draft' WHERE id = $1`, hid); err != nil {
		t.Fatal(err)
	}
	srv.forumPurge(ctx)
	if _, body, _ := get(t, fx.anon, fx.url()); strings.Contains(body, "Secret plans") || strings.Contains(body, "Staff Room") {
		t.Error("index shows an unpublished forum's topic")
	}
	if _, body, _ := get(t, fx.anon, fx.url("search")+"?q=quokka"); !strings.Contains(body, "0 results") {
		t.Error("search finds a topic of an unpublished forum")
	}
	if code, _, _ := get(t, fx.anon, secret.URL); code != http.StatusNotFound {
		t.Errorf("topic of an unpublished forum: %d", code)
	}

	// The shortcodes on a normal page.
	page := &store.Post{Type: "page", Title: "Board", Slug: "board", Status: "publish", AuthorID: 1,
		Content: `<!-- wp:shortcode -->[pursae_forum view="index"]<!-- /wp:shortcode --><!-- wp:shortcode -->[pursae_forum view="recent" limit="3"]<!-- /wp:shortcode -->`}
	if err := srv.store.InsertPost(ctx, page); err != nil {
		t.Fatal(err)
	}
	code, body, loc := get(t, fx.anon, fx.base+"/?page_id="+itoa64(page.ID))
	if code == http.StatusMovedPermanently {
		code, body, _ = get(t, fx.anon, loc)
	}
	if code != http.StatusOK {
		t.Fatalf("shortcode page: %d", code)
	}
	forumNoTemplateError(t, "shortcode page", body)
	wbtfWant(t, "shortcode page", body, `data-view="shortcode-index"`, `data-view="shortcode-recent"`, "General Chat", "Topic number 7", "forum.css")
	// The stylesheet carries the reported plugin version as its cache-buster (the plugins list shows the same constant).
	wbtfWant(t, "shortcode page", body, "/forum.css?ver="+forumVersion)
	// The recent shortcode and the theme function skip topics of the unpublished forum (the newest topic).
	if strings.Contains(body, "Secret plans") {
		t.Error(`[pursae_forum view="recent"] lists a topic of an unpublished forum`)
	}
	if h := string(srv.forumRecentTopicsHTML(ctx, nil, 3)); h == "" || strings.Contains(h, "Secret plans") {
		t.Errorf("forum_recent_topics: %s", h)
	}
}

// Posting, editing and moderation over HTTP.
func TestForumFrontPosting(t *testing.T) {
	fx := forumFrontStart(t, false)
	srv, ctx := fx.srv, fx.ctx
	newURL := fx.url("f", fx.forum.Slug, "new")

	// A visitor's POST goes to the login form.
	if code, _, loc := post(t, fx.anon, newURL, url.Values{"forum_action": {"new_topic"}, "title": {"Hi"}, "content": {"x"}}); code != http.StatusFound || !strings.Contains(loc, "/forums/login/?redirect=") {
		t.Errorf("visitor POST: %d %s", code, loc)
	}

	mara, maraID := fx.member(t, "mara", forumRoleParticipant)

	// The new topic form: no-store, the participant's nonce.
	code, body, hdr := getHdr(t, mara, newURL)
	if code != http.StatusOK || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("new topic form: %d %v", code, hdr)
	}
	forumNoTemplateError(t, "new topic", body)
	nonce := forumFormNonce(t, body, "new_topic")

	// No nonce → 403; validation errors → 422 with the values kept.
	if code, _, _ := post(t, mara, newURL, url.Values{"forum_action": {"new_topic"}, "title": {"Hello there"}, "content": {"Body"}}); code != http.StatusForbidden {
		t.Errorf("no nonce: %d", code)
	}
	code, body, _ = post(t, mara, newURL, url.Values{"forum_action": {"new_topic"}, "_wpnonce": {nonce}, "title": {"Hi"}, "content": {"Kept body text"}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "The title is too short") || !strings.Contains(body, "Kept body text") {
		t.Errorf("short title: %d", code)
	}
	if code, body, _ := post(t, mara, newURL, url.Values{"forum_action": {"new_topic"}, "_wpnonce": {nonce}, "title": {"Fine title"}, "content": {"<p> </p>"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "write a little more") {
		t.Errorf("empty content: %d", code)
	}
	if code, _, _ := post(t, mara, newURL, url.Values{"forum_action": {"new_topic"}, "_wpnonce": {nonce}, "title": {"Fine title"}, "content": {"x"}, commentHoneypot: {"http://spam"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("honeypot: %d", code)
	}

	// A topic → 302 to the topic; the forum (anonymous) shows it at once (cache purged).
	code, _, loc := post(t, mara, newURL, url.Values{"forum_action": {"new_topic"}, "_wpnonce": {nonce}, "title": {"My first topic"},
		"content": {"Hello <b>forum</b><script>alert(1)</script>"}})
	if code != http.StatusFound || !strings.Contains(loc, "/forums/t/") || !strings.Contains(loc, "/my-first-topic/") {
		t.Fatalf("new topic: %d %s", code, loc)
	}
	topics, _, _ := srv.forumTopics(ctx, forumTopicQuery{ForumID: fx.forum.ID})
	if len(topics) != 1 || topics[0].AuthorID != maraID || strings.Contains(topics[0].Content, "script") || !strings.Contains(topics[0].Content, "<b>forum</b>") {
		t.Fatalf("stored topic: %+v", topics)
	}
	topic := topics[0]
	if _, body, _ := get(t, fx.anon, fx.forum.URL); !strings.Contains(body, "My first topic") {
		t.Error("forum page does not show the new topic")
	}

	// The member's topic page: reply form, no-store; a reply → 302 to its anchor; counts updated.
	code, body, hdr = getHdr(t, mara, topic.URL)
	if code != http.StatusOK || hdr.Get("Cache-Control") != "no-store" || !strings.Contains(body, `id="reply"`) {
		t.Fatalf("member topic page: %d %v", code, hdr)
	}
	wbtfBad(t, "member topic page", body, "pf-modbar")
	nonce = forumFormNonce(t, body, "reply")
	code, _, loc = post(t, mara, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {nonce}, "content": {"A reply from Mara"}})
	if code != http.StatusFound || !regexp.MustCompile(`/forums/t/\d+/my-first-topic/#post-\d+$`).MatchString(loc) {
		t.Fatalf("reply: %d %s", code, loc)
	}
	replyID, _ := strconv.ParseInt(loc[strings.LastIndex(loc, "-")+1:], 10, 64)
	if tp, _ := srv.forumTopicByID(ctx, topic.ID); tp.ReplyCount != 1 {
		t.Errorf("topic reply count: %d", tp.ReplyCount)
	}
	if f, _ := srv.forumByID(ctx, fx.forum.ID); f.TopicCount != 1 || f.ReplyCount != 1 {
		t.Errorf("forum counts: %d %d", f.TopicCount, f.ReplyCount)
	}
	if _, body, _ := get(t, fx.anon, topic.URL); !strings.Contains(body, "A reply from Mara") || !strings.Contains(body, `id="post-`+itoa64(replyID)+`"`) {
		t.Error("reply not on the topic page")
	}
	if code, body, _ := post(t, mara, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {nonce}, "content": {"x"}}); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "write a little more") || !strings.Contains(body, `id="reply"`) {
		t.Errorf("short reply: %d", code)
	}

	// Flood control.
	fx.settings(t, func(st *forumSettings) { st.FloodSeconds = 60 })
	if code, body, _ := post(t, mara, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {nonce}, "content": {"Too fast reply"}}); code != http.StatusTooManyRequests || !strings.Contains(body, "posting too quickly") {
		t.Errorf("flood: %d", code)
	}
	fx.settings(t, func(st *forumSettings) { st.FloodSeconds = 0 })

	// Disallowed keys reject the post.
	if err := srv.settings.Set(ctx, map[string]any{"disallowed_keys": "casino\n"}); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := post(t, mara, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {nonce}, "content": {"Visit my CASINO"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "not allowed") {
		t.Errorf("disallowed keys: %d", code)
	}
	_ = srv.settings.Set(ctx, map[string]any{"disallowed_keys": ""})

	// Edit within the window; after it the form is gone and the POST refused.
	editURL := fx.url("edit", itoa64(replyID))
	code, body, _ = get(t, mara, editURL)
	if code != http.StatusOK || !strings.Contains(body, "A reply from Mara") {
		t.Fatalf("edit form: %d", code)
	}
	forumNoTemplateError(t, "edit", body)
	enonce := forumFormNonce(t, body, "edit")
	if code, _, loc := post(t, mara, editURL, url.Values{"forum_action": {"edit"}, "_wpnonce": {enonce}, "content": {"An edited reply"}}); code != http.StatusFound || !strings.HasSuffix(loc, "#post-"+itoa64(replyID)) {
		t.Errorf("edit: %d %s", code, loc)
	}
	if r, _ := srv.forumReplyByID(ctx, replyID); r.Content != "An edited reply" || r.EditedAt == nil {
		t.Errorf("edited reply: %+v", r)
	}
	topicEdit := fx.url("edit", itoa64(topic.ID))
	if code, _, loc := post(t, mara, topicEdit, url.Values{"forum_action": {"edit"}, "_wpnonce": {enonce}, "title": {"My renamed topic"}, "content": {"New lead"}}); code != http.StatusFound || !strings.Contains(loc, "/my-renamed-topic/") {
		t.Errorf("edit topic: %d %s", code, loc)
	}
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE posts SET created_at = now() - interval '2 hours', published_at = now() - interval '2 hours' WHERE id = $1`, replyID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := get(t, mara, editURL); code != http.StatusNotFound {
		t.Errorf("edit form after the window: %d", code)
	}
	if code, body, _ := post(t, mara, editURL, url.Values{"forum_action": {"edit"}, "_wpnonce": {enonce}, "content": {"Late edit"}}); code != http.StatusForbidden ||
		strings.Contains(body, `value="edit"`) || !strings.Contains(body, "Back to the topic") || !strings.Contains(body, "no longer edit") {
		t.Errorf("edit after the window: %d", code)
	}

	// A closed forum: the new-topic page says so and has no form; the POST is refused.
	closedID, err := srv.forumSaveForum(ctx, forumForumInput{Title: "Closed Board", ParentID: fx.cat, Closed: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	closed, _ := srv.forumByID(ctx, closedID)
	closedNew := fx.url("f", closed.Slug, "new")
	if code, body, _ := get(t, mara, closedNew); code != http.StatusOK || strings.Contains(body, `value="new_topic"`) || !strings.Contains(body, "closed to new topics") {
		t.Errorf("new topic in a closed forum: %d", code)
	}
	if code, _, _ := post(t, mara, closedNew, url.Values{"forum_action": {"new_topic"}, "_wpnonce": {nonce}, "title": {"Closed hello"}, "content": {"Should not land"}}); code != http.StatusForbidden {
		t.Errorf("POST to a closed forum: %d", code)
	}

	// Moderation: a participant may not; a moderator closes the topic; replies are refused for the
	// participant and accepted for the moderator.
	if code, _, _ := post(t, mara, topic.URL, url.Values{"forum_action": {"moderate"}, "_wpnonce": {nonce}, "post_id": {itoa64(topic.ID)}, "op": {"close"}}); code != http.StatusForbidden {
		t.Errorf("participant moderate: %d", code)
	}
	ozan, _ := fx.member(t, "ozan", forumRoleModerator)
	topic, _ = srv.forumTopicByID(ctx, topic.ID)
	code, body, _ = get(t, ozan, topic.URL)
	if code != http.StatusOK || !strings.Contains(body, "pf-modbar") || !strings.Contains(body, `value="close"`) {
		t.Fatalf("moderator topic page: %d", code)
	}
	mnonce := forumFormNonce(t, body, "moderate")
	if code, _, loc := post(t, ozan, topic.URL, url.Values{"forum_action": {"moderate"}, "_wpnonce": {mnonce}, "post_id": {itoa64(topic.ID)}, "op": {"close"}}); code != http.StatusFound || !strings.Contains(loc, "forum_msg=saved") {
		t.Errorf("close: %d %s", code, loc)
	}
	if code, _, _ := post(t, mara, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {nonce}, "content": {"Reply to a closed topic"}}); code != http.StatusForbidden {
		t.Errorf("reply to a closed topic: %d", code)
	}
	if _, body, _ := get(t, mara, topic.URL); strings.Contains(body, `id="reply"`) || !strings.Contains(body, "closed to new replies") {
		t.Error("closed topic still offers the reply form")
	}
	if code, _, _ := post(t, ozan, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {mnonce}, "content": {"Moderator reply"}}); code != http.StatusFound {
		t.Errorf("moderator reply to a closed topic: %d", code)
	}
	// Stick, trash a reply (hidden from visitors), restore it; unknown op 400.
	if code, _, _ := post(t, ozan, topic.URL, url.Values{"forum_action": {"moderate"}, "_wpnonce": {mnonce}, "post_id": {itoa64(topic.ID)}, "op": {"stick"}}); code != http.StatusFound {
		t.Errorf("stick: %d", code)
	}
	if tp, _ := srv.forumTopicByID(ctx, topic.ID); !tp.Sticky || !tp.Closed {
		t.Errorf("topic state: sticky %v closed %v", tp.Sticky, tp.Closed)
	}
	if code, _, _ := post(t, ozan, topic.URL, url.Values{"forum_action": {"moderate"}, "_wpnonce": {mnonce}, "post_id": {itoa64(replyID)}, "op": {"trash"}}); code != http.StatusFound {
		t.Errorf("trash: %d", code)
	}
	if _, body, _ := get(t, fx.anon, topic.URL); strings.Contains(body, "An edited reply") {
		t.Error("trashed reply visible")
	}
	if code, _, _ := post(t, ozan, topic.URL, url.Values{"forum_action": {"moderate"}, "_wpnonce": {mnonce}, "post_id": {itoa64(replyID)}, "op": {"restore"}}); code != http.StatusFound {
		t.Errorf("restore: %d", code)
	}
	if _, body, _ := get(t, fx.anon, topic.URL); !strings.Contains(body, "An edited reply") {
		t.Error("restored reply missing")
	}
	if code, _, _ := post(t, ozan, topic.URL, url.Values{"forum_action": {"moderate"}, "_wpnonce": {mnonce}, "post_id": {itoa64(topic.ID)}, "op": {"explode"}}); code != http.StatusBadRequest {
		t.Errorf("unknown op: %d", code)
	}
	// Moderators edit anything, any time.
	if code, _, _ := get(t, ozan, editURL); code != http.StatusOK {
		t.Errorf("moderator edit form: %d", code)
	}
	if code, _, _ := post(t, ozan, topic.URL, url.Values{"forum_action": {"moderate"}, "_wpnonce": {mnonce}, "post_id": {itoa64(topic.ID)}, "op": {"open"}}); code != http.StatusFound {
		t.Errorf("open: %d", code)
	}

	// Block from the profile: the member can no longer post; an editor cannot be blocked by a
	// moderator; the admin unblocks.
	code, body, _ = get(t, ozan, fx.url("u", "mara"))
	if code != http.StatusOK || !strings.Contains(body, "Block member") {
		t.Fatalf("profile as moderator: %d", code)
	}
	bnonce := forumFormNonce(t, body, "block")
	if code, _, loc := post(t, ozan, fx.url("u", "mara"), url.Values{"forum_action": {"block"}, "_wpnonce": {bnonce}}); code != http.StatusFound || !strings.Contains(loc, "forum_msg=blocked") {
		t.Errorf("block: %d %s", code, loc)
	}
	// Blocking ended Mara's sessions; she signs in again.
	var maraSessions int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, maraID).Scan(&maraSessions)
	if maraSessions != 0 {
		t.Errorf("blocked member keeps %d sessions", maraSessions)
	}
	if code, _, _ := post(t, mara, fx.base+"/wp-login", url.Values{"log": {"mara"}, "pwd": {"member password 1"}}); code != http.StatusFound {
		t.Fatalf("mara signs in again: %d", code)
	}
	_, body, _ = get(t, mara, fx.base+"/forums/account/")
	if m := regexp.MustCompile(`name="_wpnonce" value="([^"]+)"`).FindStringSubmatch(body); m != nil {
		nonce = m[1]
	} else {
		t.Fatal("no forum nonce on the account page")
	}
	if code, body, _ := post(t, mara, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {nonce}, "content": {"Am I blocked?"}}); code != http.StatusForbidden || !strings.Contains(body, "cannot post") {
		t.Errorf("blocked member posts: %d", code)
	}
	if _, body, _ := get(t, fx.anon, fx.url("u", "mara")); !strings.Contains(body, "pf-badge--blocked") {
		t.Error("profile lacks the blocked badge")
	}
	if _, err := srv.store.CreateUser(ctx, store.NewUser{Login: "eddie", Email: "eddie@example.test", PasswordHash: "x", Role: "editor"}); err != nil {
		t.Fatal(err)
	}
	if _, body, _ := get(t, ozan, fx.url("u", "eddie")); strings.Contains(body, "Block member") {
		t.Error("a moderator is offered to block an editor")
	}
	if code, _, _ := post(t, ozan, fx.url("u", "eddie"), url.Values{"forum_action": {"block"}, "_wpnonce": {bnonce}}); code != http.StatusForbidden {
		t.Errorf("moderator blocks an editor: %d", code)
	}
	if code, _, _ := post(t, mara, fx.url("u", "ozan"), url.Values{"forum_action": {"block"}, "_wpnonce": {nonce}}); code != http.StatusForbidden {
		t.Errorf("participant blocks: %d", code)
	}
	_, body, _ = get(t, fx.admin, fx.url("u", "mara"))
	anonce := forumFormNonce(t, body, "unblock")
	if code, _, _ := post(t, fx.admin, fx.url("u", "mara"), url.Values{"forum_action": {"unblock"}, "_wpnonce": {anonce}}); code != http.StatusFound {
		t.Errorf("unblock: %d", code)
	}
	// Unblocking is an access change too: Mara's sessions end, and after signing in she posts again.
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, maraID).Scan(&maraSessions)
	if maraSessions != 0 {
		t.Errorf("unblocked member keeps %d sessions", maraSessions)
	}
	if code, _, _ := post(t, mara, fx.base+"/wp-login", url.Values{"log": {"mara"}, "pwd": {"member password 1"}}); code != http.StatusFound {
		t.Fatalf("mara signs in after the unblock: %d", code)
	}
	_, body, _ = get(t, mara, fx.base+"/forums/account/")
	if m := regexp.MustCompile(`name="_wpnonce" value="([^"]+)"`).FindStringSubmatch(body); m != nil {
		nonce = m[1]
	} else {
		t.Fatal("no forum nonce on the account page after the unblock")
	}
	if code, _, loc := post(t, mara, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {nonce}, "content": {"Back again"}}); code != http.StatusFound || strings.Contains(loc, "wp-login") {
		t.Errorf("unblocked member posts: %d %s", code, loc)
	}
	if _, body, _ := get(t, fx.anon, topic.URL); !strings.Contains(body, "Back again") {
		t.Error("the unblocked member's reply is not on the topic")
	}

	// First-post moderation: the topic is pending, hidden from visitors, visible to its author, and
	// published once a moderator approves it.
	fx.settings(t, func(st *forumSettings) { st.ModerateFirstPost = true })
	nova, _ := fx.member(t, "nova", forumRoleParticipant)
	_, body, _ = get(t, nova, newURL)
	nnonce := forumFormNonce(t, body, "new_topic")
	code, _, loc = post(t, nova, newURL, url.Values{"forum_action": {"new_topic"}, "_wpnonce": {nnonce}, "title": {"Pending hello"}, "content": {"Waiting for approval"}})
	if code != http.StatusFound || !strings.Contains(loc, "forum_msg=topic_pending") {
		t.Fatalf("pending topic: %d %s", code, loc)
	}
	pend, _, _ := srv.forumTopics(ctx, forumTopicQuery{ForumID: fx.forum.ID, Statuses: []string{"pending"}})
	if len(pend) != 1 {
		t.Fatalf("pending topics: %d", len(pend))
	}
	if code, _, _ := get(t, fx.anon, pend[0].URL); code != http.StatusNotFound {
		t.Errorf("pending topic for visitors: %d", code)
	}
	if code, body, _ := get(t, nova, loc); code != http.StatusOK || !strings.Contains(body, "waiting for a moderator") {
		t.Errorf("pending topic for its author: %d", code)
	}
	if code, body, _ := get(t, ozan, fx.forum.URL); code != http.StatusOK || !strings.Contains(body, "Pending hello") || !strings.Contains(body, "pf-badge--pending") {
		t.Errorf("moderator forum list lacks the pending topic: %d", code)
	}
	if code, _, _ := post(t, ozan, pend[0].URL, url.Values{"forum_action": {"moderate"}, "_wpnonce": {mnonce}, "post_id": {itoa64(pend[0].ID)}, "op": {"approve"}}); code != http.StatusFound {
		t.Errorf("approve: %d", code)
	}
	if code, _, _ := get(t, fx.anon, pend[0].URL); code != http.StatusOK {
		t.Errorf("approved topic: %d", code)
	}

	// An unconfirmed member cannot post.
	fx.settings(t, func(st *forumSettings) { st.ModerateFirstPost = false })
	uv, uvID := fx.member(t, "unver", forumRoleParticipant)
	_, body, _ = get(t, uv, topic.URL)
	unonce := forumFormNonce(t, body, "reply")
	if err := srv.forumSetUserMeta(ctx, uvID, map[string]any{forumUMetaUnverified: true}); err != nil {
		t.Fatal(err)
	}
	_, body, _ = get(t, uv, topic.URL)
	if strings.Contains(body, `name="forum_action" value="reply"`) || !strings.Contains(body, "confirm your email") {
		t.Error("unconfirmed member sees the reply form")
	}
	if code, body, _ := post(t, uv, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {unonce}, "content": {"Unverified reply"}}); code != http.StatusForbidden || !strings.Contains(body, "confirm your email") {
		t.Errorf("unconfirmed member posts: %d", code)
	}
}

// Inside Hello Pursae with Pursae Web Builder on: a topic shows the builder header template and
// no generic Single Page main template.
func TestForumFrontHelloPursae(t *testing.T) {
	fx := forumFrontStart(t, true)
	srv, ctx := fx.srv, fx.ctx
	if err := srv.switchTheme(ctx, wbHelloSlug); err != nil {
		t.Fatal(err)
	}
	wbtfTemplate(t, srv, webbuilder.TemplateHeader, "Site header", []string{"include/general"}, `<p>FORUM-HDR-MARK</p>`, "", "", nil)
	wbtfTemplate(t, srv, webbuilder.TemplateSinglePage, "Page layout", []string{"include/singular/page"}, `<p>PAGE-TPL-MARK</p>`, "", "", nil)
	tp, err := srv.forumCreateTopic(ctx, forumNewTopic{ForumID: fx.forum.ID, AuthorID: 1, Title: "Builder topic", Content: "<p>Inside the builder theme</p>"})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{fx.url(), fx.forum.URL, tp.URL} {
		code, body, _ := get(t, fx.anon, u)
		if code != http.StatusOK {
			t.Errorf("%s: %d", u, code)
			continue
		}
		forumNoTemplateError(t, u, body)
		wbtfWant(t, u, body, "FORUM-HDR-MARK", "wp-theme-"+wbHelloSlug, `class="pf alignwide"`, "forum.css")
		wbtfBad(t, u, body, "PAGE-TPL-MARK")
	}
	if _, body, _ := get(t, fx.anon, tp.URL); !strings.Contains(body, "Inside the builder theme") {
		t.Error("topic body missing in Hello Pursae")
	}
}

// Unit: the posting rules.
func TestForumPostRules(t *testing.T) {
	if got := forumCleanTitle("  Two\n\tlines\x00 here "); got != "Two lines here" {
		t.Errorf("forumCleanTitle: %q", got)
	}
	if forumCheckTitle("ab") == "" || forumCheckTitle(strings.Repeat("x", 121)) == "" || forumCheckTitle("abc") != "" || forumCheckTitle("Çağ") != "" {
		t.Error("title limits")
	}
	if forumCheckContent("x", "x") == "" || forumCheckContent("<p> </p>", "<p> </p>") == "" || forumCheckContent("ok", "ok") != "" {
		t.Error("content minimum")
	}
	if forumCheckContent(strings.Repeat("x", 20001), "x") == "" {
		t.Error("content maximum")
	}
	if got := forumWithMsg("http://x.test/forums/t/1/a/#post-2", "saved"); got != "http://x.test/forums/t/1/a/?forum_msg=saved#post-2" {
		t.Errorf("forumWithMsg: %s", got)
	}
	if got := forumExcerpt("<p>one <b>two</b> three</p>", 2); got != "one two…" {
		t.Errorf("excerpt: %q", got)
	}
	st := forumDefaults()
	author := &store.User{ID: 5}
	if forumCanEdit(author, st, 5, time.Now(), "publish", false) {
		t.Error("a user without forum_participate edits")
	}
	if forumCanEdit(nil, st, 5, time.Now(), "publish", false) {
		t.Error("a visitor edits")
	}
	if g := forumGroupsOf([]*forumForum{{ID: 1, Kind: "category", Children: []*forumForum{{ID: 2}}}, {ID: 3, Kind: "forum"}}); len(g) != 2 || g[0].Category != nil || g[1].Category.ID != 1 {
		t.Errorf("groups: %+v", g)
	}
}

// commentLinkCount counts an anchor and its URL once (the comment_max_links rule held a comment
// with one <a href="https://…"> when the limit was 2).
func TestCommentLinkCount(t *testing.T) {
	for in, want := range map[string]int{
		"no links here":                                                       0,
		`<a href="https://a.example">a</a>`:                                   1,
		`<a href='http://a.example' rel="ugc">a</a>`:                          1,
		"see https://a.example and http://b.example":                          2,
		`<a href="https://a.example">a</a> <a href="https://b.example">b</a>`: 2,
		`<A HREF = "//a.example">a</A>`:                                       1,
		"// a code comment":                                                   0,
		`<a href="/x">a</a>`:                                                  1,
		`<a href="mailto:a@b.example">m</a>`:                                  1,
		`<a href="www.x.example">x</a>`:                                       1,
		`see https://a.example and <a href='//b.example'>b</a>`:               2,
	} {
		if got := commentLinkCount(in); got != want {
			t.Errorf("commentLinkCount(%q) = %d, want %d", in, got, want)
		}
	}
}

// The Discussion screen's comment_max_links and moderation_keys hold a member's new topic or reply
// for moderation (bbp_check_for_moderation); moderators bypass; 0 turns the link limit off.
func TestForumFrontLinkModeration(t *testing.T) {
	fx := forumFrontStart(t, false)
	srv, ctx := fx.srv, fx.ctx
	if err := srv.settings.Set(ctx, map[string]any{"comment_max_links": 2, "moderation_keys": ""}); err != nil {
		t.Fatal(err)
	}
	newURL := fx.url("f", fx.forum.Slug, "new")
	mara, _ := fx.member(t, "mara", forumRoleParticipant)
	_, body, _ := get(t, mara, newURL)
	nonce := forumFormNonce(t, body, "new_topic")

	one := `See <a href="https://a.example">a</a>`
	two := `See <a href="https://a.example">a</a> and <a href="https://b.example">b</a>`

	// One link publishes (an anchor is one link, not two).
	code, _, loc := post(t, mara, newURL, url.Values{"forum_action": {"new_topic"}, "_wpnonce": {nonce}, "title": {"One link topic"}, "content": {one}})
	if code != http.StatusFound || strings.Contains(loc, "pending") {
		t.Fatalf("one-link topic: %d %s", code, loc)
	}
	topics, _, _ := srv.forumTopics(ctx, forumTopicQuery{ForumID: fx.forum.ID})
	if len(topics) != 1 || topics[0].Status != "publish" {
		t.Fatalf("published topics: %+v", topics)
	}
	topic := topics[0]

	// Two links: the topic is held.
	code, _, loc = post(t, mara, newURL, url.Values{"forum_action": {"new_topic"}, "_wpnonce": {nonce}, "title": {"Two link topic"}, "content": {two}})
	if code != http.StatusFound || !strings.Contains(loc, "forum_msg=topic_pending") {
		t.Errorf("two-link topic: %d %s", code, loc)
	}
	if pend, _, _ := srv.forumTopics(ctx, forumTopicQuery{ForumID: fx.forum.ID, Statuses: []string{"pending"}}); len(pend) != 1 || pend[0].Title != "Two link topic" {
		t.Errorf("pending topics: %+v", pend)
	}

	// replyStatus posts a reply and returns the stored status of the newest reply and the redirect.
	replyStatus := func(t *testing.T, c *http.Client, content string) (string, string) {
		t.Helper()
		_, body, _ := get(t, c, topic.URL)
		rn := forumFormNonce(t, body, "reply")
		code, _, loc := post(t, c, topic.URL, url.Values{"forum_action": {"reply"}, "_wpnonce": {rn}, "content": {content}})
		if code != http.StatusFound {
			t.Fatalf("reply: %d", code)
		}
		var st string
		if err := srv.store.Pool.QueryRow(ctx, `SELECT status FROM posts WHERE type = 'reply' ORDER BY id DESC LIMIT 1`).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st, loc
	}

	if st, loc := replyStatus(t, mara, one); st != "publish" || strings.Contains(loc, "pending") {
		t.Errorf("one-link reply: %s %s", st, loc)
	}
	if st, loc := replyStatus(t, mara, two); st != "pending" || !strings.Contains(loc, "forum_msg=reply_pending") {
		t.Errorf("two-link reply: %s %s", st, loc)
	}
	ozan, _ := fx.member(t, "ozan", forumRoleModerator)
	if st, loc := replyStatus(t, ozan, two); st != "publish" || strings.Contains(loc, "pending") {
		t.Errorf("moderator two-link reply: %s %s", st, loc)
	}

	// comment_max_links 0 turns the check off.
	if err := srv.settings.Set(ctx, map[string]any{"comment_max_links": 0}); err != nil {
		t.Fatal(err)
	}
	if st, loc := replyStatus(t, mara, two); st != "publish" || strings.Contains(loc, "pending") {
		t.Errorf("two-link reply with no limit: %s %s", st, loc)
	}

	// A moderation key holds the post too.
	if err := srv.settings.Set(ctx, map[string]any{"moderation_keys": "pharmacy\n"}); err != nil {
		t.Fatal(err)
	}
	if st, loc := replyStatus(t, mara, "Cheap PHARMACY deals"); st != "pending" || !strings.Contains(loc, "forum_msg=reply_pending") {
		t.Errorf("moderation key reply: %s %s", st, loc)
	}
	_ = srv.settings.Set(ctx, map[string]any{"moderation_keys": "", "comment_max_links": 2})
}
