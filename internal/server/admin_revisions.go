package server

import (
	"context"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// revisionMeta is what wp_prepare_revisions_for_js() hands the "From" / "To" cards: author, avatar,
// relative time and the short date.
type revisionMeta struct {
	ID               int64
	Author, Avatar   template.HTML
	TimeAgo, Date    string
	Current, Missing bool // Missing: comparing against "no revision"
}

type revisionOption struct {
	ID    int64
	Label string
}

type diffField struct {
	ID, Name string
	Diff     template.HTML
}

type revisionData struct {
	Post       *store.Post
	Title      string
	EditURL    string
	From, To   revisionMeta
	Options    []revisionOption // newest first, for the from/to selectors
	FromID     int64
	ToID       int64
	Fields     []diffField
	RestoreURL string
	CanRestore bool
	PrevURL    string // the older revision (Previous)
	NextURL    string // the newer revision (Next)
}

// revisionScreen is revision.php: compare two revisions of a post and restore one.
func (s *Server) revisionScreen(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	id, _ := strconv.ParseInt(c.Query("revision"), 10, 64)
	rev, err := s.store.GetRevision(ctx, id)
	if err != nil {
		c.String(http.StatusNotFound, "Invalid revision ID.")
		return
	}
	p, err := s.store.GetPost(ctx, rev.PostID)
	if err != nil || !u.Can("edit_post", p) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit this item.")
		return
	}
	if c.Query("action") == "restore" {
		if !s.checkNonce(c, "restore-post_"+itoa64(rev.ID)) {
			s.denyNonce(c)
			return
		}
		if _, err := s.store.RestoreRevision(ctx, rev, u.ID); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		p, _ = s.store.GetPost(ctx, p.ID)
		s.afterPostSave(ctx, p)
		c.Redirect(http.StatusFound, links.EditLink(p.ID)+"&message=5&revision="+itoa64(rev.ID))
		return
	}

	revs, _ := s.store.ListRevisions(ctx, p.ID, false)
	d := revisionData{Post: p, Title: defaultStr(p.Title, "(no title)"), EditURL: links.EditLink(p.ID), ToID: rev.ID}
	var from *store.Revision
	if fid, _ := strconv.ParseInt(c.Query("from"), 10, 64); fid > 0 {
		if f, err := s.store.GetRevision(ctx, fid); err == nil && f.PostID == p.ID {
			from = f
		}
	}
	for i, r := range revs {
		d.Options = append(d.Options, revisionOption{r.ID, s.revisionTitle(ctx, r, i == 0)})
		if r.ID == rev.ID {
			if i+1 < len(revs) {
				d.PrevURL = "/wp-admin/revision?revision=" + itoa64(revs[i+1].ID)
				if from == nil {
					from = revs[i+1] // the revision before the one being viewed
				}
			}
			if i > 0 {
				d.NextURL = "/wp-admin/revision?revision=" + itoa64(revs[i-1].ID)
			}
		}
	}
	d.To = s.revisionMetaFor(ctx, rev, len(revs) > 0 && revs[0].ID == rev.ID)
	d.CanRestore = !d.To.Current
	d.RestoreURL = "/wp-admin/revision?revision=" + itoa64(rev.ID) + "&action=restore&_wpnonce=" + s.nonce(c, "restore-post_"+itoa64(rev.ID))
	fromTitle, fromContent, fromExcerpt := "", "", ""
	if from != nil {
		d.From = s.revisionMetaFor(ctx, from, false)
		d.FromID = from.ID
		fromTitle, fromContent, fromExcerpt = from.Title, from.Content, from.Excerpt
	} else {
		d.From = revisionMeta{Missing: true}
	}
	// wp_get_revision_ui_diff(): the title always shows, content and excerpt only when they changed.
	title := wp.TextDiff(fromTitle, rev.Title)
	if title == "" {
		title = `<table class="diff"><colgroup><col class="content diffsplit left" /><col class="content diffsplit middle" /><col class="content diffsplit right" /></colgroup><tbody><tr><td>` +
			template.HTMLEscapeString(fromTitle) + `</td><td></td><td>` + template.HTMLEscapeString(rev.Title) + `</td></tr></tbody></table>`
	}
	d.Fields = append(d.Fields, diffField{"post_title", "Title", template.HTML(title)})
	if diff := wp.TextDiff(fromContent, rev.Content); diff != "" {
		d.Fields = append(d.Fields, diffField{"post_content", "Content", template.HTML(diff)})
	}
	if diff := wp.TextDiff(fromExcerpt, rev.Excerpt); diff != "" {
		d.Fields = append(d.Fields, diffField{"post_excerpt", "Excerpt", template.HTML(diff)})
	}
	s.adminPage(c, "revision", "Compare Revisions of “"+d.Title+"”", "revision-php", "admin/revision", d)
}

// revisionMetaFor fills a From / To card.
func (s *Server) revisionMetaFor(ctx context.Context, r *store.Revision, current bool) revisionMeta {
	loc := s.loc()
	m := revisionMeta{ID: r.ID, Current: current, TimeAgo: wp.HumanTimeDiff(r.CreatedAt, time.Now()) + " ago",
		Date: wp.FormatDate(r.CreatedAt.In(loc), "j M @ H:i")}
	m.Author, m.Avatar = s.revisionAuthor(ctx, r.AuthorID, 48)
	return m
}

// revisionAuthor is the author name and avatar of a revision.
func (s *Server) revisionAuthor(ctx context.Context, authorID int64, size int) (template.HTML, template.HTML) {
	if u, err := s.store.UserByID(ctx, authorID); err == nil {
		return template.HTML(template.HTMLEscapeString(u.DisplayName)), template.HTML(s.avatarImg(ctx, u.Email, size))
	}
	return "", ""
}

// revisionTitle is wp_post_revision_title(): "October 27, 2026 @ 10:00", marked when it is the
// current revision or an autosave.
func (s *Server) revisionTitle(ctx context.Context, r *store.Revision, latest bool) string {
	date := wp.FormatDate(r.CreatedAt.In(s.loc()), "F j, Y @ H:i")
	if r.IsAutosave {
		return date + " [Autosave]"
	}
	if latest {
		return date + " [Current Revision]"
	}
	return date
}

// revisionsListHTML is wp_list_post_revisions(): the Revisions metabox of the editor.
func (s *Server) revisionsListHTML(c *gin.Context, revs []*store.Revision) template.HTML {
	if len(revs) == 0 {
		return ""
	}
	loc := s.loc()
	out := "<ul class=\"post-revisions hide-if-no-js\">\n"
	for _, r := range revs {
		name, avatar := s.revisionAuthor(c, r.AuthorID, 24)
		date := wp.FormatDate(r.CreatedAt.In(loc), "F j, Y @ H:i")
		link := "<a href='/wp-admin/revision?revision=" + itoa64(r.ID) + "'>" + date + "</a>"
		item := string(avatar) + " " + string(name) + ", " + link
		if r.IsAutosave {
			item += " [Autosave]"
		}
		out += "\t<li>" + item + "</li>\n"
	}
	return template.HTML(out + "</ul>")
}
