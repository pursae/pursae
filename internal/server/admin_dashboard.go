package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

type activityItem struct {
	Title, EditURL, When, ISO, Excerpt string
}

type dashboardComment struct {
	ID                                    int64
	Author, AuthorURL, Avatar, Status     string
	PostTitle, PostEditURL, Link, Excerpt string
}

type dashboardData struct {
	ShowWelcome                       bool
	Posts, Pages, Comments            int
	Pending, Spam, Trash, AllComments int
	Discouraged                       bool
	Future, Published, Drafts         []activityItem
	RecentComments                    []dashboardComment
	QuickDraftNonce                   string
	ThemeName                         string
	Health                            healthSummary
}

// healthSummary is what the Site Health Status widget shows: the counts of the Site Health tests
// and the score site-health.js's recalculateProgression() draws from them.
type healthSummary struct {
	Good, Recommended, Critical int
	Issues                      int     // recommended + critical
	Class                       string  // green or orange (loading when there are no tests)
	Label                       string  // Good / Should be improved
	Offset                      float64 // stroke-dashoffset of the progress circle
}

// siteHealthSummary runs the checks of the Site Health screen (siteHealth in admin_misc.go) that
// do not need the screen's details and scores them the way site-health.js does.
func (s *Server) siteHealthSummary(ctx context.Context) healthSummary {
	var h healthSummary
	good := func() { h.Good++ }
	recommended := func() { h.Recommended++ }
	good() // current WordPress Go version
	good() // Go runtime
	var pgVersion string
	_ = s.store.Pool.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion)
	if pgMajor, _ := strconv.Atoi(strings.SplitN(pgVersion, ".", 2)[0]); pgMajor >= 15 {
		good()
	} else {
		recommended()
	}
	if strings.HasPrefix(s.links(ctx).Home, "https://") {
		good()
	} else {
		recommended()
	}
	if f, err := os.CreateTemp(s.cfg.UploadsDir, ".health-*"); err == nil {
		f.Close()
		os.Remove(f.Name())
		good()
	} else {
		h.Critical++
	}
	if s.settings.Bool("wpgo_cache_enabled") {
		good()
	} else {
		recommended()
	}
	if s.plugins != nil && s.plugins.initErr == nil {
		good()
	} else {
		h.Critical++
	}
	if s.settings.String("timezone_string") != "" {
		good()
	} else {
		recommended()
	}
	if !s.settings.Bool("blog_public") {
		recommended()
	}
	var inactivePlugins int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM plugins WHERE status = 'inactive'`).Scan(&inactivePlugins)
	if inactivePlugins > 0 {
		recommended()
	}
	if s.settings.Int("wp_page_for_privacy_policy") == 0 {
		recommended()
	}
	h.Issues = h.Recommended + h.Critical
	// site-health.js recalculateProgression().
	total := float64(h.Good) + float64(h.Recommended) + 1.5*float64(h.Critical)
	failed := 0.5*float64(h.Recommended) + 1.5*float64(h.Critical)
	val := 100
	if total > 0 {
		val = 100 - int(math.Ceil(failed/total*100))
	}
	val = max(0, min(100, val))
	h.Offset = math.Round(float64(100-val)/100*math.Pi*180*100) / 100 // r=90: circumference 565.48
	h.Class, h.Label = "orange", "Should be improved"
	if val >= 80 && h.Critical == 0 {
		h.Class, h.Label = "green", "Good"
	}
	return h
}

func (s *Server) dashboard(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("edit_posts", nil) {
		c.Redirect(http.StatusFound, "/wp-admin/profile")
		return
	}
	if w := c.Query("welcome"); w == "0" || w == "1" {
		_ = s.setUserMeta(c, u, "show_welcome_panel", w == "1")
		c.Redirect(http.StatusFound, "/wp-admin/")
		return
	}
	ctx := c.Request.Context()
	d := dashboardData{QuickDraftNonce: s.nonce(c, "add-post"), Discouraged: !s.settings.Bool("blog_public"), ThemeName: s.activeTheme().Name}
	if u.Can("manage_options", nil) {
		d.Health = s.siteHealthSummary(ctx)
	} else {
		d.Health = healthSummary{Class: "loading", Label: "No information yet\u2026"}
	}
	d.ShowWelcome = true
	if v, ok := u.Meta["show_welcome_panel"].(bool); ok {
		d.ShowWelcome = v
	}

	postCounts, _ := s.store.StatusCounts(ctx, "post", 0)
	pageCounts, _ := s.store.StatusCounts(ctx, "page", 0)
	commentCounts, _ := s.store.CommentCounts(ctx)
	d.Posts, d.Pages = postCounts["publish"], pageCounts["publish"]
	d.Comments, d.Pending = commentCounts["approved"], commentCounts["pending"]
	d.Spam, d.Trash = commentCounts["spam"], commentCounts["trash"]
	d.AllComments = d.Comments + d.Pending

	loc := s.loc()
	when := func(t time.Time) string {
		t = t.In(loc)
		now := time.Now().In(loc)
		y, m, dd := now.Date()
		ty, tm, td := t.Date()
		tf := s.settings.String("time_format")
		switch {
		case y == ty && m == tm && dd == td:
			return "Today, " + wp.FormatDate(t, tf)
		case now.AddDate(0, 0, 1).Format("2006-01-02") == t.Format("2006-01-02"):
			return "Tomorrow, " + wp.FormatDate(t, tf)
		case y == ty:
			return wp.FormatDate(t, "M jS") + " " + wp.FormatDate(t, tf)
		}
		return wp.FormatDate(t, "M jS Y") + " " + wp.FormatDate(t, tf)
	}
	title := func(p *store.Post) string {
		if strings.TrimSpace(p.Title) == "" {
			return "(no title)"
		}
		return p.Title
	}

	future, _, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"post"}, Statuses: []string{"future"}, OrderBy: "date", Order: "asc", PerPage: 5})
	for _, p := range future {
		d.Future = append(d.Future, activityItem{Title: title(p), EditURL: links.EditLink(p.ID), When: when(p.Date())})
	}
	published, _, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, PerPage: 5})
	for _, p := range published {
		d.Published = append(d.Published, activityItem{Title: title(p), EditURL: links.EditLink(p.ID), When: when(p.Date())})
	}
	drafts, _, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"post"}, Statuses: []string{"draft"}, AuthorID: u.ID, OrderBy: "modified", PerPage: 3})
	for _, p := range drafts {
		d.Drafts = append(d.Drafts, activityItem{
			Title: title(p), EditURL: links.EditLink(p.ID), ISO: p.ModifiedAt.In(loc).Format(time.RFC3339),
			When: wp.FormatDate(p.ModifiedAt.In(loc), "F j, Y"), Excerpt: wp.TrimWords(p.Content, 10, "&hellip;"),
		})
	}

	lb := s.links(ctx)
	comments, _ := s.store.RecentComments(ctx, 5, u.Can("moderate_comments", nil))
	for _, cm := range comments {
		p, err := s.store.GetPost(ctx, cm.PostID)
		if err != nil {
			continue
		}
		status := "approved"
		if cm.Status == "pending" {
			status = "unapproved"
		}
		author := cm.AuthorName
		if author == "" {
			author = "Anonymous"
		}
		d.RecentComments = append(d.RecentComments, dashboardComment{
			ID: cm.ID, Author: author, AuthorURL: cm.AuthorURL, Avatar: s.avatarURL(c, cm.AuthorEmail, 50), Status: status,
			PostTitle: title(p), PostEditURL: links.EditLink(p.ID), Link: lb.Permalink(p) + "#comment-" + itoa64(cm.ID),
			Excerpt: wp.TrimWords(cm.Content, 20, "&hellip;"),
		})
	}

	s.adminPage(c, "", "Dashboard", "index-php", "admin/dashboard", d)
}

// quickDraft saves the Quick Draft box (wp_dashboard_quick_press()).
func (s *Server) quickDraft(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("edit_posts", nil) {
		c.Status(http.StatusForbidden)
		return
	}
	if !s.checkNonce(c, "add-post") {
		s.denyNonce(c)
		return
	}
	title := strings.TrimSpace(c.PostForm("post_title"))
	content := strings.TrimSpace(c.PostForm("content"))
	if title != "" || content != "" {
		p := &store.Post{Type: "post", AuthorID: u.ID, Status: "draft", Title: title, Content: paragraphsToBlocks(content)}
		if err := s.store.InsertPost(c, p); err != nil {
			c.String(http.StatusInternalServerError, "Could not save draft.")
			return
		}
		_ = s.store.SetPostTerms(c, p.ID, "category", []int64{int64(s.settings.Int("default_category"))})
	}
	c.Redirect(http.StatusFound, "/wp-admin/")
}

// setUserMeta merges one key into users.meta and the in-memory user.
func (s *Server) setUserMeta(c *gin.Context, u *store.User, key string, value any) error {
	u.Meta[key] = value
	raw, _ := json.Marshal(map[string]any{key: value})
	_, err := s.store.Pool.Exec(c, `UPDATE users SET meta = meta || $2::jsonb WHERE id = $1`, u.ID, raw)
	return err
}

// paragraphsToBlocks wraps plain text the way the editor would store it: one paragraph block per paragraph.
// Content that already has block delimiters is kept as is.
func paragraphsToBlocks(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if strings.TrimSpace(text) == "" || strings.Contains(text, "<!-- wp:") {
		return text
	}
	var parts []string
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if strings.HasPrefix(para, "<!--more") && strings.HasSuffix(para, "-->") {
			parts = append(parts, "<!-- wp:more -->\n"+para+"\n<!-- /wp:more -->")
			continue
		}
		para = strings.ReplaceAll(para, "\n", "<br>")
		parts = append(parts, "<!-- wp:paragraph -->\n<p>"+para+"</p>\n<!-- /wp:paragraph -->")
	}
	return strings.Join(parts, "\n\n")
}
