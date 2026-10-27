package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"wpgo/internal/wp"
)

// attr finds the value of the first attribute matching the regexp in body.
func attr(t *testing.T, body, re string) string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no match for %s", re)
	}
	return m[1]
}

// installClassic installs the site, logs in and switches to the classic editor.
func installClassic(t *testing.T, c *http.Client, base string) {
	t.Helper()
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Lists"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK || !strings.Contains(body, "Success!") {
		t.Fatalf("install failed: %d", code)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login failed: %d", code)
	}
	_, body, _ := get(t, c, base+"/wp-admin/options-writing")
	defCat := attr(t, body, `value="(\d+)" selected="selected"`)
	if code, _, _ := post(t, c, base+"/wp-admin/options", url.Values{"option_page": {"writing"}, "_wpnonce": {formNonce(t, body)},
		"default_category": {defCat}, "default_post_format": {"0"}, "classic-editor-replace": {"classic"}, "classic-editor-allow-users": {"disallow"}}); code != http.StatusFound {
		t.Fatalf("writing settings: %d", code)
	}
}

// editorPost creates a post (or page) through the classic editor form and returns its id.
func editorPost(t *testing.T, c *http.Client, base, typ, title, content string, extra url.Values) string {
	t.Helper()
	_, body, _ := get(t, c, base+"/wp-admin/post-new?post_type="+typ)
	id := attr(t, body, `name="post_ID" value="(\d+)"`)
	form := url.Values{"_wpnonce": {formNonce(t, body)}, "post_ID": {id}, "publish": {"Publish"}, "visibility": {"public"},
		"comment_status": {"open"}, "ping_status": {"open"}, "post_title": {title}, "content": {content}}
	for k, v := range extra {
		form[k] = v
	}
	if code, _, loc := post(t, c, base+"/wp-admin/post", form); code != http.StatusFound || !strings.Contains(loc, "message=") {
		t.Fatalf("publish %s: %d %s", title, code, loc)
	}
	return id
}

func TestListScreensQuickEditBulkEditAndScreenOptions(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	base := ts.URL
	installClassic(t, c, base)
	pool := getPool(t)
	ctx := context.Background()

	idA := editorPost(t, c, base, "post", "Alpha post", "Alpha body.", url.Values{"tax_input[post_tag]": {"golang"}})
	idB := editorPost(t, c, base, "post", "Beta post", "Beta body with more words than one.", url.Values{"save": {"Save Draft"}, "publish": {""}})
	pageID := editorPost(t, c, base, "page", "About", "About us.", nil)

	// edit.php: Quick Edit / Bulk Edit rows, the hidden inline data and the Screen Options tab.
	_, body, _ := get(t, c, base+"/wp-admin/edit")
	for _, want := range []string{`id="inline-edit" class="inline-edit-row inline-edit-row-post quick-edit-row quick-edit-row-post inline-edit-post"`,
		`id="bulk-edit" class="inline-edit-row inline-edit-row-post bulk-edit-row bulk-edit-row-post bulk-edit-post"`,
		`id="_inline_edit" name="_inline_edit"`, `<div class="hidden" id="inline_` + idA + `">`, `class="button-link editinline"`,
		`<option value="edit" class="hide-if-no-js">Edit</option>`, `name="bulk_edit" id="bulk_edit"`, `inline-edit-post.min.js`,
		`id="screen-options-wrap"`, `id="edit_post_per_page"`, `name="wp_screen_options[option]" value="edit_post_per_page"`,
		`class="hide-column-tog" name="author-hide"`, `id="show-settings-link"`, `id="excerpt-view-mode"`, `<div class="tags_input" id="post_tag_` + idA + `">golang</div>`,
		`table-view-list posts`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit.php misses %q", want)
		}
	}
	inlineNonce := attr(t, body, `id="_inline_edit" name="_inline_edit" value="([^"]+)"`)
	bulkNonce := formNonce(t, body)
	catID := attr(t, body, `<div class="post_category" id="category_`+idA+`">(\d+)</div>`)

	// inline-save: Quick Edit returns the re-rendered row.
	_, row, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"inline-save"}, "_inline_edit": {inlineNonce}, "post_ID": {idA}, "post_type": {"post"},
		"screen": {"edit-post"}, "post_view": {"list"}, "edit_date": {"true"}, "post_title": {"Alpha quick"}, "post_name": {"alpha-quick"},
		"mm": {"03"}, "jj": {"04"}, "aa": {"2024"}, "hh": {"10"}, "mn": {"30"}, "ss": {"00"}, "_status": {"publish"}, "comment_status": {"open"},
		"post_category[]": {catID}, "tax_input[post_tag]": {"golang, lists"}, "sticky": {"sticky"}, "post_format": {"aside"}, "post_password": {""}})
	if !strings.HasPrefix(strings.TrimSpace(row), `<tr id="post-`+idA+`"`) || !strings.Contains(row, "Alpha quick") || !strings.Contains(row, `<span class="post-state">Sticky</span>`) ||
		!strings.Contains(row, `<div class="post_format">aside</div>`) || !strings.Contains(row, "format-aside") {
		t.Errorf("inline-save row = %q", row)
	}
	var title, slug, format, pingStatus string
	var sticky bool
	_ = pool.QueryRow(ctx, `SELECT title, slug, format, sticky, ping_status FROM posts WHERE id = $1`, idA).Scan(&title, &slug, &format, &sticky, &pingStatus)
	if title != "Alpha quick" || slug != "alpha-quick" || format != "aside" || !sticky || pingStatus != "closed" {
		t.Errorf("after inline-save: %q %q %q sticky=%v pings=%q", title, slug, format, sticky, pingStatus)
	}
	var tagCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM post_terms pt JOIN terms t ON t.id = pt.term_id WHERE pt.post_id = $1 AND t.taxonomy = 'post_tag'`, idA).Scan(&tagCount)
	if tagCount != 2 {
		t.Errorf("tags after inline-save = %d, want 2", tagCount)
	}
	if _, bad, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"inline-save"}, "_inline_edit": {"nope"}, "post_ID": {idA}, "post_type": {"post"}}); bad != "-1" {
		t.Errorf("inline-save without a nonce = %q", bad)
	}

	// The Sticky view and the post-format filter appear now that a sticky, aside-format post exists.
	_, body, _ = get(t, c, base+"/wp-admin/edit")
	if !strings.Contains(body, `<li class="sticky"> |<a href="/wp-admin/edit?show_sticky=1">Sticky <span class="count">(1)</span></a></li>`) ||
		!strings.Contains(body, `<select name="post_format" id="filter-by-format">`) || !strings.Contains(body, `value="aside">Aside</option>`) {
		t.Errorf("edit.php lacks the Sticky view or the format filter")
	}
	_, body, _ = get(t, c, base+"/wp-admin/edit?show_sticky=1")
	if !strings.Contains(body, "Alpha quick") || strings.Contains(body, "Beta post") || !strings.Contains(body, `<li class="sticky"> |<a href="/wp-admin/edit?show_sticky=1" class="current"`) {
		t.Errorf("show_sticky view wrong")
	}
	_, body, _ = get(t, c, base+"/wp-admin/edit?post_format=aside")
	if !strings.Contains(body, "Alpha quick") || strings.Contains(body, "Beta post") {
		t.Errorf("post_format filter wrong")
	}

	// Extended view (mode=excerpt) shows the excerpt under the title and remembers the choice in the user settings cookie.
	_, body, _ = get(t, c, base+"/wp-admin/edit?mode=excerpt")
	if !strings.Contains(body, `table-view-excerpt posts`) || !strings.Contains(body, "Beta body with more words than one.") || !strings.Contains(body, `id="excerpt-view-mode" type="radio" name="mode" value="excerpt" checked="checked"`) {
		t.Errorf("extended view not rendered")
	}
	_, body, _ = get(t, c, base+"/wp-admin/edit")
	if !strings.Contains(body, `table-view-excerpt posts`) {
		t.Errorf("list mode not remembered")
	}
	get(t, c, base+"/wp-admin/edit?mode=list")

	// Bulk Edit: Update submits the list form with action=edit.
	code, _, loc := get(t, c, base+"/wp-admin/edit?post_type=post&_wpnonce="+bulkNonce+"&action=edit&bulk_edit=Update&post%5B%5D="+idA+"&post%5B%5D="+idB+
		"&_status=draft&comment_status=closed&ping_status=&post_author=-1&sticky=-1&post_format=-1&tax_input%5Bpost_tag%5D=bulk")
	if code != http.StatusFound || !strings.Contains(loc, "updated=2") {
		t.Fatalf("bulk edit: %d %s", code, loc)
	}
	var statusA, commentsA string
	_ = pool.QueryRow(ctx, `SELECT status, comment_status FROM posts WHERE id = $1`, idA).Scan(&statusA, &commentsA)
	if statusA != "draft" || commentsA != "closed" {
		t.Errorf("after bulk edit: status=%q comments=%q", statusA, commentsA)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM post_terms pt JOIN terms t ON t.id = pt.term_id WHERE pt.post_id = $1 AND t.taxonomy = 'post_tag'`, idA).Scan(&tagCount)
	if tagCount != 3 {
		t.Errorf("tags after bulk edit = %d, want 3 (added, not replaced)", tagCount)
	}
	_, body, _ = get(t, c, base+loc)
	if !strings.Contains(body, "2 posts updated.") {
		t.Errorf("bulk edit notice missing")
	}

	// Screen Options: items per page through the form, hidden columns through admin-ajax.
	soNonce := attr(t, body, `id="screenoptionnonce" name="screenoptionnonce" value="([^"]+)"`)
	req, _ := http.NewRequest(http.MethodPost, base+"/wp-admin/edit", strings.NewReader(url.Values{"wp_screen_options[option]": {"edit_post_per_page"},
		"wp_screen_options[value]": {"1"}, "screenoptionnonce": {soNonce}, "mode": {"list"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", base+"/wp-admin/edit?paged=2")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/wp-admin/edit" {
		t.Errorf("set-screen-option: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, body, _ = get(t, c, base+"/wp-admin/edit")
	if !strings.Contains(body, `id="edit_post_per_page" maxlength="3" value="1"`) || !strings.Contains(body, `<span class="total-pages">3</span>`) {
		t.Errorf("per-page option not applied")
	}
	if _, ok, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"hidden-columns"}, "hidden": {"author,date"}, "page": {"edit-post"}, "screenoptionnonce": {soNonce}}); ok != "1" {
		t.Errorf("hidden-columns = %q", ok)
	}
	_, body, _ = get(t, c, base+"/wp-admin/edit")
	if !strings.Contains(body, `class="manage-column column-author hidden sortable`) || !strings.Contains(body, `class="author column-author hidden"`) ||
		!strings.Contains(body, `class="hide-column-tog" name="author-hide" type="checkbox" id="author-hide" value="author" />Author`) {
		t.Errorf("hidden columns not applied")
	}
	post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"hidden-columns"}, "hidden": {""}, "page": {"edit-post"}, "screenoptionnonce": {soNonce}})

	// Pages: Quick Edit carries parent, order and template; the page tree depth survives inline-save.
	_, body, _ = get(t, c, base+"/wp-admin/edit?post_type=page")
	if !strings.Contains(body, `<div class="post_parent">0</div>`) || !strings.Contains(body, `<select name="post_parent">`) || !strings.Contains(body, `name="menu_order" class="inline-edit-menu-order-input"`) {
		t.Errorf("page quick edit fields missing")
	}
	inlineNonce = attr(t, body, `id="_inline_edit" name="_inline_edit" value="([^"]+)"`)
	_, row, _ = post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"inline-save"}, "_inline_edit": {inlineNonce}, "post_ID": {pageID}, "post_type": {"page"},
		"post_title": {"About us"}, "post_name": {"about"}, "_status": {"publish"}, "post_parent": {"0"}, "menu_order": {"3"}, "page_template": {"default"}, "comment_status": {"open"}, "ping_status": {"open"},
		"mm": {"03"}, "jj": {"04"}, "aa": {"2024"}, "hh": {"10"}, "mn": {"30"}, "ss": {"00"}})
	if !strings.Contains(row, `<div class="menu_order">3</div>`) || !strings.Contains(row, "About us") {
		t.Errorf("page inline-save row = %q", row)
	}

	// edit-tags.php: Quick Edit row and inline-save-tax.
	_, body, _ = get(t, c, base+"/wp-admin/edit-tags?taxonomy=post_tag")
	for _, want := range []string{`<tr id="inline-edit" class="inline-edit-row"`, `<input type="text" name="name" class="ptitle"`, `inline-edit-tax.min.js`,
		`class="button-link editinline"`, `id="edit_post_tag_per_page"`, `name="taxonomy" value="post_tag"`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit-tags.php misses %q", want)
		}
	}
	tagID := attr(t, body, `<tr id="tag-(\d+)" class="level-0">`)
	taxNonce := attr(t, body, `id="_inline_edit" name="_inline_edit" value="([^"]+)"`)
	_, row, _ = post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"inline-save-tax"}, "_inline_edit": {taxNonce}, "tax_type": {"tag"}, "tax_ID": {tagID},
		"taxonomy": {"post_tag"}, "post_type": {"post"}, "name": {"Renamed tag"}, "slug": {"renamed"}})
	if !strings.HasPrefix(strings.TrimSpace(row), `<tr id="tag-`+tagID+`"`) || !strings.Contains(row, "Renamed tag") || !strings.Contains(row, `<div class="slug">renamed</div>`) {
		t.Errorf("inline-save-tax row = %q", row)
	}
	if _, bad, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"inline-save-tax"}, "_inline_edit": {taxNonce}, "tax_ID": {tagID}, "taxonomy": {"post_tag"}, "name": {""}}); !strings.Contains(bad, "A name is required") {
		t.Errorf("inline-save-tax with no name = %q", bad)
	}
	if _, names, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"ajax-tag-search"}, "tax": {"post_tag"}, "q": {"Ren"}}); names != "Renamed tag" {
		t.Errorf("ajax-tag-search = %q", names)
	}
}

func TestCommentsReplyQuickEditAndDashboard(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	base := ts.URL
	installClassic(t, c, base)
	pool := getPool(t)
	ctx := context.Background()

	var helloID string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM posts WHERE type = 'post' AND status = 'publish' ORDER BY id LIMIT 1`).Scan(&helloID)
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	post(t, anon, base+"/wp-comments-post", url.Values{"comment_post_ID": {helloID}, "comment": {"Pending question"}, "author": {"Visitor"}, "email": {"v@example.test"}})
	var pendingID string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM comments WHERE content = 'Pending question'`).Scan(&pendingID)

	_, body, _ := get(t, c, base+"/wp-admin/edit-comments")
	for _, want := range []string{`<tr id="replyrow" style="display:none;">`, `id="_ajax_nonce-replyto-comment"`, `edit-comments.min.js`, `quicktags({id:"replycontent"`,
		`data-comment-id="` + pendingID + `" data-post-id="` + helloID + `" data-action="replyto" class="vim-r comment-inline button-link"`,
		`data-action="edit" class="vim-q comment-inline button-link"`, `<div id="inline-` + pendingID + `" class="hidden">`, `<div class="comment_status">0</div>`,
		`data-wp-lists="dim:the-comment-list:comment-` + pendingID + `:unapproved:e7e7d3:e7e7d3:new=approved"`, `name="_total"`, `id="edit_comments_per_page"`,
		`orderby=comment_author`, `orderby=comment_date`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit-comments.php misses %q", want)
		}
	}
	if _, sorted, _ := get(t, c, base+"/wp-admin/edit-comments?orderby=comment_author&order=asc"); !strings.Contains(sorted, `column-author sorted asc`) {
		t.Errorf("author column not marked sorted")
	}
	replyNonce := attr(t, body, `id="_ajax_nonce-replyto-comment" name="_ajax_nonce-replyto-comment" value="([^"]+)"`)

	// replyto-comment from the list approves the pending parent and answers with WP_Ajax_Response XML.
	_, xml, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"replyto-comment"}, "_ajax_nonce-replyto-comment": {replyNonce}, "comment_ID": {pendingID},
		"comment_post_ID": {helloID}, "content": {"Thanks for asking"}, "approve_parent": {"1"}, "position": {"-1"}, "mode": {""}, "checkbox": {"1"}})
	if !strings.HasPrefix(xml, "<?xml version='1.0' encoding='utf-8' standalone='yes'?><wp_ajax><response action='replyto-comment_") ||
		!strings.Contains(xml, "<parent_approved><![CDATA["+pendingID+"]]></parent_approved>") || !strings.Contains(xml, "Thanks for asking") || !strings.Contains(xml, `<tr id="comment-`) {
		t.Errorf("replyto-comment = %q", xml)
	}
	var parentStatus, replyStatus, replyAuthor, replyParent string
	_ = pool.QueryRow(ctx, `SELECT status FROM comments WHERE id = $1`, pendingID).Scan(&parentStatus)
	_ = pool.QueryRow(ctx, `SELECT status, author_name, coalesce(parent_id,0)::text FROM comments WHERE content = 'Thanks for asking'`).Scan(&replyStatus, &replyAuthor, &replyParent)
	if parentStatus != "approved" || replyStatus != "approved" || replyAuthor != "admin" || replyParent != pendingID {
		t.Errorf("reply: parent=%q reply=%q by %q parent_id=%q", parentStatus, replyStatus, replyAuthor, replyParent)
	}
	if _, empty, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"replyto-comment"}, "_ajax_nonce-replyto-comment": {replyNonce}, "comment_post_ID": {helloID}, "content": {" "}}); empty != "Please type your comment text." {
		t.Errorf("empty reply = %q", empty)
	}

	// edit-comment: Quick Edit of the comment row.
	_, xml, _ = post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"edit-comment"}, "_ajax_nonce-replyto-comment": {replyNonce}, "comment_ID": {pendingID},
		"comment_post_ID": {helloID}, "content": {"Edited question"}, "newcomment_author": {"Visitor Two"}, "newcomment_author_email": {"v2@example.test"}, "newcomment_author_url": {""}, "status": {"1"}, "position": {"-1"}})
	if !strings.Contains(xml, "<response action='edit-comment_"+pendingID+"'><edit_comment id='"+pendingID+"'") || !strings.Contains(xml, "Edited question") || !strings.Contains(xml, "Visitor Two") {
		t.Errorf("edit-comment = %q", xml)
	}

	// dim-comment / delete-comment: the row actions through wp-lists.js.
	approveNonce := attr(t, body, `c=`+pendingID+`&amp;action=unapprovecomment&amp;_wpnonce=([0-9a-f]+)`)
	_, xml, _ = post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"dim-comment"}, "id": {pendingID}, "new": {"unapproved"}, "_ajax_nonce": {approveNonce}})
	_ = pool.QueryRow(ctx, `SELECT status FROM comments WHERE id = $1`, pendingID).Scan(&parentStatus)
	if parentStatus != "pending" || !strings.Contains(xml, "<response action='dim-comment_"+pendingID+"'>") || !strings.Contains(xml, "<status><![CDATA[0]]></status>") {
		t.Errorf("dim-comment: status=%q xml=%q", parentStatus, xml)
	}
	deleteNonce := attr(t, body, `c=`+pendingID+`&amp;action=spamcomment&amp;_wpnonce=([0-9a-f]+)`)
	_, xml, _ = post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"delete-comment"}, "id": {pendingID}, "trash": {"1"}, "_ajax_nonce": {deleteNonce}})
	_ = pool.QueryRow(ctx, `SELECT status FROM comments WHERE id = $1`, pendingID).Scan(&parentStatus)
	if parentStatus != "trash" || !strings.Contains(xml, "<status><![CDATA[trash]]></status>") {
		t.Errorf("delete-comment: status=%q xml=%q", parentStatus, xml)
	}
	if _, bad, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"delete-comment"}, "id": {pendingID}, "delete": {"1"}, "_ajax_nonce": {"nope"}}); bad != "-1" {
		t.Errorf("delete-comment without a nonce = %q", bad)
	}

	// Screen Options on the comments list posts to edit-comments.php.
	soNonce := attr(t, body, `id="screenoptionnonce" name="screenoptionnonce" value="([^"]+)"`)
	if code, _, loc := post(t, c, base+"/wp-admin/edit-comments", url.Values{"wp_screen_options[option]": {"edit_comments_per_page"}, "wp_screen_options[value]": {"7"}, "screenoptionnonce": {soNonce}}); code != http.StatusFound {
		t.Errorf("comments screen options: %d %s", code, loc)
	}
	if _, body, _ := get(t, c, base+"/wp-admin/edit-comments"); !strings.Contains(body, `id="edit_comments_per_page" maxlength="3" value="7"`) {
		t.Errorf("comments per-page not saved")
	}

	// Dashboard: the activity Edit link opens comment.php, the theme name is the active theme and
	// Site Health Status is scored.
	_, body, _ = get(t, c, base+"/wp-admin/")
	if !strings.Contains(body, `href="/wp-admin/comment?action=editcomment&amp;c=`) || !strings.Contains(body, `running <a href="/wp-admin/themes">Twenty Sixteen</a> theme.`) {
		t.Errorf("dashboard activity Edit link or theme name wrong")
	}
	if !regexp.MustCompile(`site-health-progress-wrapper (green|orange) hide-if-no-js`).MatchString(body) || !strings.Contains(body, `stroke-dashoffset="`) ||
		!(strings.Contains(body, "Should be improved") || strings.Contains(body, `site-health-progress-label">Good<`)) || !strings.Contains(body, "on the <a href=\"/wp-admin/site-health\">Site Health screen</a>") {
		t.Errorf("dashboard site health widget not scored")
	}
}

func TestRevisionsAndEditorMetaboxes(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	base := ts.URL
	installClassic(t, c, base)
	pool := getPool(t)
	ctx := context.Background()

	id := editorPost(t, c, base, "post", "Draft one", "First line.\n\nSecond line.", nil)
	update := func(title, content string, extra url.Values) string {
		t.Helper()
		_, body, _ := get(t, c, base+"/wp-admin/post?post="+id+"&action=edit")
		form := url.Values{"_wpnonce": {formNonce(t, body)}, "post_ID": {id}, "save": {"Update"}, "post_status": {"publish"}, "visibility": {"public"},
			"comment_status": {"open"}, "post_title": {title}, "content": {content}, "post_name": {"draft-one"}}
		for k, v := range extra {
			form[k] = v
		}
		if code, _, loc := post(t, c, base+"/wp-admin/post", form); code != http.StatusFound {
			t.Fatalf("update: %d %s", code, loc)
		}
		return body
	}
	update("Draft two", "First line.\n\nSecond line changed.", nil)
	update("Draft three", "First line.\n\nSecond line changed.\n\nThird line.", nil)

	// The editor: Revisions box and publish-box row, hidden metaboxes, Custom Fields, nonces.
	_, body, _ := get(t, c, base+"/wp-admin/post?post="+id+"&action=edit")
	for _, want := range []string{`id="revisionsdiv" class="postbox hide-if-js"`, `<ul class="post-revisions hide-if-no-js">`, `Revisions: <b>2</b>`,
		`<span aria-hidden="true">Browse</span>`, `id="postcustom" class="postbox hide-if-js"`, `id="metakeyinput" name="metakeyinput"`, `data-wp-lists="add:the-list:newmeta"`,
		`id="trackbacksdiv" class="postbox hide-if-js"`, `name="trackback_url" id="trackback_url"`, `id="commentstatusdiv" class="postbox hide-if-js"`,
		`id="slugdiv" class="postbox hide-if-js"`, `id="closedpostboxesnonce"`, `class="hide-postbox-tog" name="slugdiv-hide"`, `id="categorydiv" class="postbox">`} {
		if !strings.Contains(body, want) {
			t.Errorf("post.php misses %q", want)
		}
	}
	latest := attr(t, body, `href="/wp-admin/revision\?revision=(\d+)"><span aria-hidden="true">Browse</span>`)
	var count int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM post_revisions WHERE post_id = $1 AND NOT is_autosave`, id).Scan(&count)
	if count != 2 { // the first save of an auto-draft records no revision
		t.Fatalf("revisions = %d, want 2", count)
	}

	// revision.php: the diff of the latest revision against the one before it.
	_, rev, _ := get(t, c, base+"/wp-admin/revision?revision="+latest)
	for _, want := range []string{`Compare Revisions of &#8220;<a href="/wp-admin/post?post=` + id + `&amp;action=edit">Draft three</a>&#8221;`, `&larr; Go to editor`,
		`<table class="diff is-split-view">`, `<td class="diff-deletedline">`, `<td class="diff-addedline">`, `<td class="diff-context">`, `<del>two</del>`, `<ins>three</ins>`,
		`<h3>Title</h3>`, `<h3>Content</h3>`, `Current Revision by <span class="author-name">admin</span>`, `disabled="disabled" class="restore-revision button button-primary"`} {
		if !strings.Contains(rev, want) {
			t.Errorf("revision.php misses %q", want)
		}
	}
	if strings.Contains(rev, "<h3>Excerpt</h3>") {
		t.Errorf("unchanged excerpt should not be in the diff")
	}
	var firstRev string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM post_revisions WHERE post_id = $1 ORDER BY created_at ASC, id ASC LIMIT 1`, id).Scan(&firstRev)
	_, rev, _ = get(t, c, base+"/wp-admin/revision?revision="+firstRev)
	if !strings.Contains(rev, `class="restore-revision button button-primary" href="/wp-admin/revision?revision=`+firstRev+`&amp;action=restore&amp;_wpnonce=`) || !strings.Contains(rev, "No earlier revision") {
		t.Errorf("oldest revision screen wrong")
	}
	restoreURL := strings.ReplaceAll(attr(t, rev, `class="restore-revision button button-primary" href="([^"]+)"`), "&amp;", "&")
	if code, _, _ := get(t, c, base+"/wp-admin/revision?revision="+firstRev+"&action=restore&_wpnonce=bad"); code != http.StatusForbidden {
		t.Errorf("restore without a nonce: %d", code)
	}
	code, _, loc := get(t, c, base+restoreURL)
	if code != http.StatusFound || !strings.Contains(loc, "message=5") {
		t.Fatalf("restore: %d %s", code, loc)
	}
	var title, content string
	_ = pool.QueryRow(ctx, `SELECT title, content FROM posts WHERE id = $1`, id).Scan(&title, &content)
	if title != "Draft two" || content != "First line.\n\nSecond line changed." {
		t.Errorf("after restore: %q %q", title, content)
	}
	if _, body, _ := get(t, c, base+loc); !strings.Contains(body, "Post restored to revision from") || !strings.Contains(body, "Revisions: <b>3</b>") {
		t.Errorf("restore notice or new revision missing")
	}
	if code, _, _ := get(t, c, base+"/wp-admin/revision?revision=999999"); code != http.StatusNotFound {
		t.Errorf("unknown revision: %d", code)
	}

	// Custom Fields: add-meta / delete-meta through admin-ajax and the edit form.
	_, body, _ = get(t, c, base+"/wp-admin/post?post="+id+"&action=edit")
	metaNonce := attr(t, body, `id="_ajax_nonce-add-meta" name="_ajax_nonce-add-meta" value="([^"]+)"`)
	_, xml, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"add-meta"}, "_ajax_nonce-add-meta": {metaNonce}, "post_id": {id}, "metakeyinput": {"color"}, "metavalue": {"blue"}})
	if !strings.HasPrefix(xml, "<?xml") || !strings.Contains(xml, "<response action='add-meta_") || !strings.Contains(xml, `name='meta[`) || !strings.Contains(xml, "value='color'") {
		t.Errorf("add-meta = %q", xml)
	}
	if _, bad, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"add-meta"}, "_ajax_nonce-add-meta": {metaNonce}, "post_id": {id}, "metakeyinput": {"_secret"}, "metavalue": {"x"}}); bad != "-1" {
		t.Errorf("protected key accepted: %q", bad)
	}
	_, body, _ = get(t, c, base+"/wp-admin/post?post="+id+"&action=edit")
	if !strings.Contains(body, "value='color'") || !strings.Contains(body, ">blue</textarea>") {
		t.Errorf("custom field not listed")
	}
	rowID := attr(t, body, `<tr id='meta-(\d+)'>`)
	// Update through the row's Update button (add-meta with meta[id][...]).
	_, xml, _ = post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"add-meta"}, "_ajax_nonce-add-meta": {metaNonce}, "post_id": {id}, "meta[" + rowID + "][key]": {"color"}, "meta[" + rowID + "][value]": {"green"}})
	if !strings.Contains(xml, "old_id='"+rowID+"'") || !strings.Contains(xml, ">green</textarea>") {
		t.Errorf("update meta = %q", xml)
	}
	var meta string
	_ = pool.QueryRow(ctx, `SELECT meta->>'color' FROM posts WHERE id = $1`, id).Scan(&meta)
	if meta != "green" {
		t.Errorf("meta after update = %q", meta)
	}
	delNonce := attr(t, body, `delete:the-list:meta-`+rowID+`::_ajax_nonce=([0-9a-f]+)`)
	if _, ok, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"delete-meta"}, "id": {rowID}, "post_id": {id}, "_ajax_nonce": {delNonce}}); ok != "1" {
		t.Errorf("delete-meta = %q", ok)
	}
	var hasColor bool
	_ = pool.QueryRow(ctx, `SELECT meta ? 'color' FROM posts WHERE id = $1`, id).Scan(&hasColor)
	if hasColor {
		t.Errorf("meta not deleted")
	}
	// The edit form adds a field, keeps underscore keys and saves trackbacks and the slug box.
	update("Draft two", "First line.\n\nSecond line changed.", url.Values{"metakeyinput": {"mood"}, "metavalue": {"happy"}, "trackback_url": {"https://a.example/x https://b.example/y"}})
	var mood, toPing string
	_ = pool.QueryRow(ctx, `SELECT meta->>'mood', meta->>'_to_ping' FROM posts WHERE id = $1`, id).Scan(&mood, &toPing)
	if mood != "happy" || toPing != "https://a.example/x\nhttps://b.example/y" {
		t.Errorf("form meta: mood=%q to_ping=%q", mood, toPing)
	}
	_, body, _ = get(t, c, base+"/wp-admin/post?post="+id+"&action=edit")
	if strings.Contains(body, "value='_to_ping'") || !strings.Contains(body, `value="https://a.example/x https://b.example/y"`) {
		t.Errorf("protected meta listed or trackbacks not shown")
	}

	// closed-postboxes stores Screen Options' metabox state; hidden boxes then render without hide-if-js.
	closedNonce := attr(t, body, `id="closedpostboxesnonce" name="closedpostboxesnonce" value="([^"]+)"`)
	if _, ok, _ := post(t, c, base+"/wp-admin/admin-ajax", url.Values{"action": {"closed-postboxes"}, "closedpostboxesnonce": {closedNonce}, "page": {"post"}, "closed": {"categorydiv"}, "hidden": {"slugdiv"}}); ok != "1" {
		t.Errorf("closed-postboxes = %q", ok)
	}
	_, body, _ = get(t, c, base+"/wp-admin/post?post="+id+"&action=edit")
	if !strings.Contains(body, `id="categorydiv" class="postbox closed"`) || !strings.Contains(body, `id="postcustom" class="postbox">`) || !strings.Contains(body, `id="slugdiv" class="postbox hide-if-js"`) {
		t.Errorf("metabox state not applied")
	}

	// Page Attributes: the Template select lists the active theme's page templates (Twenty Twenty has one).
	_, themes, _ := get(t, c, base+"/wp-admin/themes")
	activate := strings.ReplaceAll(attr(t, themes, `href="(/wp-admin/themes\?action=activate&amp;stylesheet=twentytwenty&amp;_wpnonce=[^"]+)"`), "&amp;", "&")
	get(t, c, base+activate)
	pageID := editorPost(t, c, base, "page", "Wide", "Wide page.", url.Values{"page_template": {"templates/template-full-width.php"}})
	var tpl string
	_ = pool.QueryRow(ctx, `SELECT meta->>'_wp_page_template' FROM posts WHERE id = $1`, pageID).Scan(&tpl)
	if tpl != "templates/template-full-width.php" {
		t.Errorf("page template = %q", tpl)
	}
	_, body, _ = get(t, c, base+"/wp-admin/post?post="+pageID+"&action=edit")
	if !strings.Contains(body, `<select name="page_template" id="page_template">`) || !strings.Contains(body, `<option value="templates/template-full-width.php" selected="selected">Full Width Template</option>`) {
		t.Errorf("template select missing")
	}
	_, body, _ = get(t, c, base+"/wp-admin/edit?post_type=page")
	if !strings.Contains(body, `<div class="page_template">templates/template-full-width.php</div>`) || !strings.Contains(body, `<select name="page_template">`) {
		t.Errorf("page list quick edit lacks the template")
	}
	_, body, _ = get(t, c, base+"/wp-admin/")
	if !strings.Contains(body, `running <a href="/wp-admin/themes">Twenty Twenty</a> theme.`) {
		t.Errorf("dashboard does not name the active theme")
	}
}

func TestTextDiff(t *testing.T) {
	if wp.TextDiff("same\ntext", "same\ntext") != "" {
		t.Errorf("identical texts must give no diff")
	}
	d := wp.TextDiff("one\ntwo three\nfour", "one\ntwo tree\nfour\nfive")
	for _, want := range []string{`<table class="diff is-split-view">`, `<td class="diff-context"><span class="screen-reader-text">Unchanged: </span>one</td>`,
		`<td class="diff-deletedline"><span aria-hidden="true" class="dashicons dashicons-minus"></span><span class="screen-reader-text">Deleted: </span>two <del>three</del></td>`,
		`<td class="diff-addedline"><span aria-hidden="true" class="dashicons dashicons-plus"></span><span class="screen-reader-text">Added: </span>two <ins>tree</ins></td>`,
		`<tr><td>&nbsp;</td><td class="diff-addedline">`, `five</td></tr>`} {
		if !strings.Contains(d, want) {
			t.Errorf("diff misses %q in\n%s", want, d)
		}
	}
	if d := wp.TextDiff("", "<b>new</b>"); !strings.Contains(d, "&lt;b&gt;new&lt;/b&gt;") {
		t.Errorf("diff must escape HTML: %s", d)
	}
}
