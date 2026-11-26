package server

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"wpgo/internal/caps"
	"wpgo/internal/store"
)

// Pursae Forum is a native, bbPress-class discussion board: forum categories and forums, topics and
// replies (all three are posts: types forum, topic and reply), front-end membership (email sign-up
// with confirmation, Google sign-in), moderation (stick, close, approve, spam, trash, block) and an
// admin screen. Every forum view renders as a synthetic page of the active theme, so any theme shows
// it; a theme declaring supports["pursae-forum"] may wrap it (templates/forum.html) and override
// the view templates (templates/forum/<view>.html).
//
//   forum.go          constants, settings, activation, the Plugins row, menu, routes
//   forum_types.go    the domain types and forumPage (the dot of every view template)
//   forum_store.go    data access: lists, create, moderate, recount, purge
//   forum_urls.go     URL building and path parsing
//   forum_render.go   the synthetic page render path, theme overrides, shortcodes
//   forum_front.go    the site intercept: GET views and POST forms
//   forum_funcs.go    template functions for themes (forum_url, forum_user_menu, …)
//   forum_views.go    the forum front end (index, forum, topic, …)
//   forum_account.go  login, registration, email confirmation, account, Google sign-in routes
//   forum_admin.go    the admin screen (Forums › Forums, Moderation, Settings, Google sign-in)
//   forum_demo.go     the theme demo "forum" section
//   rest_forum.go     forum/v1 read-only REST routes
//
// Like every native plugin it is off on a fresh install (native_pursae_forum); while it is off every
// forum URL is a 404, the menu is gone and the template functions print nothing. Data stays.

const (
	forumSlug            = "pursae-forum"
	forumSetting         = "native_pursae_forum"   // bool, off on fresh install
	forumOptions         = "pursae_forum_settings" // JSON
	forumVersion         = "0.2.6"
	forumNonce           = "pursae-forum"      // logged-in front actions
	forumAuthNonce       = "pursae-forum-auth" // login/register/resend (logged-out nonce, pages are no-store)
	forumAdminNonce      = "pursae-forum-admin"
	forumCacheTag        = "forum" // every forum view; purged on every forum write
	forumCtxRender       = "forum.front.render"
	forumRenderTag       = "pursae_forum_render"
	forumRenderMarker    = "<!-- wp:shortcode -->[" + forumRenderTag + "]<!-- /wp:shortcode -->"
	forumShortcodeTag    = "pursae_forum" // [pursae_forum view="index|recent" limit="5"] on normal pages
	forumAuthPrefix      = "/forum-auth"  // gin routes, fixed so the Google redirect URI never changes
	forumAssetsPrefix    = "/forum-assets"
	forumOAuthCookie     = "wpgo_forum_oauth"
	forumThemeSupport    = "pursae-forum" // theme.json supports key
	forumRoleParticipant = "forum_participant"
	forumRoleModerator   = "forum_moderator"
	capForumParticipate  = "forum_participate"
	capForumModerate     = "forum_moderate"
)

// Meta keys of forum posts (posts.meta) and members (users.meta).
const (
	forumMetaKind         = "_forum_kind"
	forumMetaStatus       = "_forum_status"
	forumMetaTopicCount   = "_forum_topic_count"
	forumMetaReplyCount   = "_forum_reply_count"
	forumMetaLastActive   = "_forum_last_active"
	forumMetaLastTopic    = "_forum_last_topic_id"
	forumMetaLastReply    = "_forum_last_reply_id"
	forumMetaLastAuthor   = "_forum_last_author_id"
	forumMetaForumID      = "_forum_forum_id"
	forumMetaSpam         = "_forum_spam"
	forumMetaIP           = "_forum_author_ip"
	forumMetaEdited       = "_forum_edited_at"
	forumUMetaUnverified  = "forum_unverified"
	forumUMetaVerify      = "forum_verify"
	forumUMetaGoogleSub   = "forum_google_sub"
	forumUMetaGoogleEmail = "forum_google_email"
	// forumUMetaNoPassword marks an account whose password nobody knows (made by Google sign-in, or
	// reset when Google was linked to an unproven address): the account page then sets a password
	// without the current one. Every other account, wherever it was made, needs its current password.
	forumUMetaNoPassword = "forum_no_password"
	// forumUMetaEmailVerified holds the address the member proved they own (the confirmation link, or
	// Google vouching for it). It counts only while it still equals users.email.
	forumUMetaEmailVerified = "forum_email_verified"

	// v0.2.5: forum design meta (forum posts), topic state, member preferences.
	forumMetaIcon       = "_forum_icon"          // forum: a forum_icon name
	forumMetaColor      = "_forum_color"         // forum: sky|mint|peach|lilac|grey
	forumMetaLabel      = "_forum_label"         // forum: short tab label
	forumMetaNumber     = "_forum_number"        // forum: short number ("01")
	forumMetaChips      = "_forum_chips"         // forum: [names]
	forumMetaTips       = "_forum_tips"          // forum: [lines]
	forumMetaStaffOnly  = "_forum_staff_only"    // forum: only moderators start topics
	forumMetaModerators = "_forum_moderators"    // forum: [user ids] shown as the forum's moderators
	forumMetaTags       = "_forum_tags"          // topic: [tag slugs]
	forumMetaTagNames   = "_forum_tag_names"     // topic: [tag names as typed], same order
	forumMetaSolution   = "_forum_solution"      // topic: the accepted reply id
	forumMetaSolvedAt   = "_forum_solved_at"     // topic: when it was accepted
	forumMetaSolvedBy   = "_forum_solved_by"     // topic: who accepted it
	forumMetaLikesSeed  = "_forum_likes_seed"    // topic/reply: demo likes added to the counted ones
	forumUMetaRules     = "forum_rules_accepted" // users.meta: when the member accepted the community rules (RFC 3339)
	forumUMetaDigest    = "forum_digest"         // users.meta: wants the weekly digest
	forumUMetaDraft     = "forum_draft"          // users.meta: the new-topic draft (forumDraft)
)

// forumTimeLayout is the fixed-width UTC layout of _forum_last_active (lexical order = time order).
const forumTimeLayout = "2006-01-02T15:04:05Z"

// forumCapServer is the server whose plugin state the forum type capabilities follow (one per
// process; set by registerForum).
var forumCapServer atomic.Pointer[Server]

// forumCapsOn reports whether the forum type capabilities apply: only while the plugin is on, so
// a same-named post type of another source (a Pursae Custom Fields "topic") keeps the core rules.
func forumCapsOn() bool {
	s := forumCapServer.Load()
	return s != nil && s.forumOn()
}

func init() {
	caps.RegisterTypeCapWhen("forum", "manage_options", forumCapsOn)
	caps.RegisterTypeCapWhen("topic", capForumModerate, forumCapsOn)
	caps.RegisterTypeCapWhen("reply", capForumModerate, forumCapsOn)
}

// Settings ----------------------------------------------------------------------------------------

// forumSettings are the plugin's settings (Forums › Settings and Google sign-in).
type forumSettings struct {
	Base               string `json:"base"`                 // "forums"; [a-z0-9-]{1,40}, not a reserved core path
	Title              string `json:"title"`                // "Forums"
	TopicsPerPage      int    `json:"topics_per_page"`      // 20 (5..100)
	RepliesPerPage     int    `json:"replies_per_page"`     // 20 (5..100)
	Registration       bool   `json:"registration"`         // true: email signup + Google account creation
	RequireVerify      bool   `json:"require_verify"`       // true: unverified members cannot post
	DefaultRole        string `json:"default_role"`         // "forum_participant"; only forum_participant|subscriber accepted
	ModerateFirstPost  bool   `json:"moderate_first_post"`  // false
	EditWindow         int    `json:"edit_window"`          // minutes, 30; 0 = members cannot edit
	FloodSeconds       int    `json:"flood_seconds"`        // 15
	GoogleEnabled      bool   `json:"google_enabled"`       //
	GoogleClientID     string `json:"google_client_id"`     //
	GoogleClientSecret string `json:"google_client_secret"` // sealed; never rendered back; empty on save = keep stored
	ShowMembers        bool   `json:"show_members"`         // WP7: true: the public member list /<base>/members/
	// v0.2.5.
	RulesURL      string `json:"rules_url"`      // the community rules page ("" = none); the sign-up checkbox links it
	RequireRules  bool   `json:"require_rules"`  // sign-up needs the rules accepted
	OnlineMinutes int    `json:"online_minutes"` // the "online" window, 5 (1..60)
	Notify        bool   `json:"notify"`         // topic subscriptions email new replies (default true)
	// OGImage is the social-share image of the public forum pages ("" = Pursae SEO's default image):
	// a site path (/wp-content/uploads/…) or an http(s) address.
	OGImage string `json:"og_image"`
}

func forumDefaults() forumSettings {
	return forumSettings{Base: "forums", Title: "Forums", TopicsPerPage: 20, RepliesPerPage: 20, Registration: true,
		RequireVerify: true, DefaultRole: forumRoleParticipant, EditWindow: 30, FloodSeconds: 15, ShowMembers: true,
		OnlineMinutes: 5, Notify: true}
}

var forumBaseRe = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// forumReservedBases are first path segments the forum base may not take (core and plugin routes).
var forumReservedBases = map[string]bool{
	"wp-admin": true, "wp-json": true, "wp-content": true, "wp-includes": true, "wp-login": true, "wp-login.php": true,
	"wp-comments-post": true, "wp-cron": true, "wp-sitemap": true, "feed": true, "page": true, "comments": true,
	"search": true, "author": true, "category": true, "tag": true, "type": true, "embed": true, "attachment": true,
	"forum-auth": true, "forum-assets": true, "shop": true, "cart": true, "checkout": true, "my-account": true,
	"product": true, "product-category": true, "product-tag": true, "web-builder": true, "oauth": true, "mcp": true,
}

func forumClamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// forumSanitize clamps the settings to what the forum supports.
func forumSanitize(st forumSettings) forumSettings {
	d := forumDefaults()
	st.Base = strings.Trim(strings.ToLower(strings.TrimSpace(st.Base)), "/")
	if !forumBaseRe.MatchString(st.Base) || forumReservedBases[st.Base] || strings.HasPrefix(st.Base, "wp-") {
		st.Base = d.Base
	}
	st.Title = strings.TrimSpace(st.Title)
	if st.Title == "" {
		st.Title = d.Title
	}
	if len([]rune(st.Title)) > 80 {
		st.Title = string([]rune(st.Title)[:80])
	}
	st.TopicsPerPage = forumClamp(st.TopicsPerPage, 5, 100, d.TopicsPerPage)
	st.RepliesPerPage = forumClamp(st.RepliesPerPage, 5, 100, d.RepliesPerPage)
	if st.DefaultRole != forumRoleParticipant && st.DefaultRole != "subscriber" {
		st.DefaultRole = forumRoleParticipant
	}
	if st.EditWindow < 0 {
		st.EditWindow = 0
	}
	if st.EditWindow > 10080 {
		st.EditWindow = 10080
	}
	if st.FloodSeconds < 0 {
		st.FloodSeconds = 0
	}
	if st.FloodSeconds > 3600 {
		st.FloodSeconds = 3600
	}
	st.OnlineMinutes = forumClamp(st.OnlineMinutes, 1, 60, d.OnlineMinutes)
	st.RulesURL = strings.TrimSpace(st.RulesURL)
	if st.RulesURL != "" && !forumRulesURLOK(st.RulesURL) {
		st.RulesURL = ""
	}
	if st.RulesURL == "" {
		st.RequireRules = false // nothing to accept
	}
	st.OGImage = strings.TrimSpace(st.OGImage)
	if st.OGImage != "" && !forumRulesURLOK(st.OGImage) {
		st.OGImage = ""
	}
	st.GoogleClientID = strings.TrimSpace(st.GoogleClientID)
	st.GoogleClientSecret = strings.TrimSpace(st.GoogleClientSecret)
	return st
}

// forumRulesURLOK accepts a site path ("/rules/") or an http(s) URL as the rules link.
func forumRulesURLOK(u string) bool {
	if len(u) > 500 || strings.ContainsAny(u, "\"<> \t\n") {
		return false
	}
	return (strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//")) || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

func (s *Server) forumOn() bool { return s.settings.Bool(forumSetting) }

// forumLoad reads the settings over the defaults.
func (s *Server) forumLoad() forumSettings {
	st := forumDefaults()
	if v := s.settings.Get(forumOptions); v != nil {
		if raw, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(raw, &st) // keys the saved settings lack keep their defaults
		}
	}
	st = forumSanitize(st)
	st.GoogleClientSecret = s.revealSecret(st.GoogleClientSecret) // stored sealed (forumSave)
	return st
}

// forumSave stores the settings; an empty Google client secret keeps the stored one. The secret
// is stored sealed (sealSecret); forumLoad opens it.
func (s *Server) forumSave(ctx context.Context, st forumSettings) error {
	st = forumSanitize(st)
	if st.GoogleClientSecret == "" {
		st.GoogleClientSecret = s.forumLoad().GoogleClientSecret
	}
	st.GoogleClientSecret = s.sealSecret(st.GoogleClientSecret)
	if err := s.settings.Set(ctx, map[string]any{forumOptions: st}); err != nil {
		return err
	}
	s.purgeAll(ctx) // the base, titles and menus of every page may change
	return nil
}

// forumGoogleReady reports whether Google sign-in can run: plugin on, enabled, client id and secret.
func (s *Server) forumGoogleReady() bool {
	if !s.forumOn() {
		return false
	}
	st := s.forumLoad()
	return st.GoogleEnabled && st.GoogleClientID != "" && st.GoogleClientSecret != ""
}

// Activation --------------------------------------------------------------------------------------

// forumActivate switches the plugin on (idempotent): the forum roles, the forum capabilities of the
// existing roles, the default settings.
func (s *Server) forumActivate(ctx context.Context) error {
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO roles (slug, name, is_system, capabilities) VALUES
		('forum_participant', 'Forum Participant', false, '{"read":true,"level_0":true,"forum_participate":true}'),
		('forum_moderator', 'Forum Moderator', false, '{"read":true,"level_0":true,"forum_participate":true,"forum_moderate":true}')
		ON CONFLICT (slug) DO NOTHING`); err != nil {
		return err
	}
	if _, err := s.store.Pool.Exec(ctx, `UPDATE roles SET capabilities = capabilities || '{"forum_participate":true}'::jsonb
		WHERE slug IN ('subscriber','contributor','author','customer')`); err != nil {
		return err
	}
	if _, err := s.store.Pool.Exec(ctx, `UPDATE roles SET capabilities = capabilities || '{"forum_participate":true,"forum_moderate":true}'::jsonb
		WHERE slug IN ('administrator','editor','shop_manager')`); err != nil {
		return err
	}
	set := map[string]any{forumSetting: true}
	if s.settings.Get(forumOptions) == nil {
		set[forumOptions] = forumDefaults()
	}
	if err := s.settings.Set(ctx, set); err != nil {
		return err
	}
	s.purgeAll(ctx)
	s.forumCacheReconfigure()
	return nil
}

// forumCacheQueryStrings are the page cache key parameters the forum adds while it is on.
var forumCacheQueryStrings = []string{"sort", "tag"}

// forumCacheReconfigure rebuilds a running page cache so the forum's key parameters follow the
// plugin's state.
func (s *Server) forumCacheReconfigure() {
	if s.currentCache() != nil {
		s.configureCache()
	}
}

// forumDeactivate switches it off; forums, topics, replies, roles and members stay.
func (s *Server) forumDeactivate(ctx context.Context) error {
	if err := s.settings.Set(ctx, map[string]any{forumSetting: false}); err != nil {
		return err
	}
	s.purgeAll(ctx)
	s.forumCacheReconfigure()
	return nil
}

// forumOnly answers 404 while the plugin is off.
func (s *Server) forumOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.forumOn() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// Plugins screen ------------------------------------------------------------------------------------

// forumPluginRow is the plugin's row on the Plugins screen.
func (s *Server) forumPluginRow() pluginRow {
	r := pluginRow{Slug: forumSlug, Name: "Pursae Forum", Version: forumVersion, Native: true, Active: s.forumOn(),
		Like:        "bbPress",
		Description: "A fast discussion board inside your theme: forum categories, forums, topics and replies, member sign-up with email confirmation or Google, moderation (sticky, closed, approve, spam, trash, block) and themes made for communities. No JavaScript needed.",
		Author:      "Pursae"}
	if r.Active {
		r.SettingsURL = "/wp-admin/forum?tab=settings"
	}
	return r
}

// forumPluginAction is the Activate / Deactivate link of the Plugins screen.
func (s *Server) forumPluginAction(c *gin.Context, action string) {
	var err error
	if action == "activate" {
		err = s.forumActivate(c)
	} else {
		err = s.forumDeactivate(c)
	}
	if err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+template.URLQueryEscaper(err.Error()))
		return
	}
	c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
}

// Admin menu ------------------------------------------------------------------------------------------

// forumMenu adds the Forums menu after Comments while the plugin is active, for moderators. Its
// submenu is the admin screen's tabs (forumAdminTabs) the user may open: Overview, Moderation and
// Members for moderators, every tab for administrators (manage_options).
func (s *Server) forumMenu(menu []*menuTop, u *store.User, screen string) []*menuTop {
	if !s.forumOn() || u == nil || !u.Can(capForumModerate, nil) {
		return menu
	}
	top := &menuTop{Slug: "forum?tab=" + forumTabOverview, Title: "Forums", ID: "toplevel_page_pursae-forum", Icon: "dashicons-format-chat"}
	for _, t := range forumAdminTabs {
		if !u.Can(t.Cap, nil) {
			continue
		}
		slug := "forum?tab=" + t.Key
		cur := slug == screen || (screen == "forum" && slug == top.Slug)
		ms := &menuSub{Slug: slug, Title: template.HTML(template.HTMLEscapeString(t.Label)), First: len(top.Sub) == 0, Current: cur}
		top.Current = top.Current || ms.Current
		top.Sub = append(top.Sub, ms)
	}
	at := len(menu)
	for i, m := range menu {
		if m.ID == "menu-comments" {
			at = i + 1
			break
		}
	}
	out := make([]*menuTop, 0, len(menu)+1)
	out = append(out, menu[:at]...)
	out = append(out, top)
	out = append(out, menu[at:]...)
	for i, m := range out {
		if m.Separator {
			continue
		}
		m.First = i == 0 || out[i-1].Separator
		m.Last = i == len(out)-1 || out[i+1].Separator
	}
	return out
}

// Routes ----------------------------------------------------------------------------------------------

// registerForum wires the plugin's gin routes; every one answers 404 while it is off. The forum's
// own pages are served through site() (forumSiteIntercept), not gin routes.
func (s *Server) registerForum(r *gin.Engine, admin *gin.RouterGroup) {
	forumCapServer.Store(s)
	s.static(r, forumAssetsPrefix, "forum/assets", s.forumOnly())
	s.registerForumAuth(r) // Google sign-in (forum_account.go)
	admin.GET("/forum", s.forumOnly(), s.forumAdmin)
	admin.POST("/forum", s.forumOnly(), s.forumAdmin)
}
