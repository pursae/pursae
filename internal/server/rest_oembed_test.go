package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestOEmbedProvider covers WordPress as an oEmbed provider: the /oembed/1.0/embed endpoint (JSON and
// XML, parameter validation, unknown URLs), the /<post>/embed/ template and the discovery links.
func TestOEmbedProvider(t *testing.T) {
	ts, c := testServer(t)
	base := ts.URL
	if code, _, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Embed Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK {
		t.Fatalf("install: %d", code)
	}
	pool := getPool(t)
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO posts (type, author_id, slug, title, content, status, published_at) VALUES ('post', 1, 'embed-me', 'Embed me', '<p>First words of the post.</p>', 'publish', now()) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	// "Post name" permalinks (Settings › Permalinks), so the embed URL is /<slug>/embed/.
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	setPermalinks := func(structure string) {
		t.Helper()
		_, form, _ := get(t, c, base+"/wp-admin/options-permalink")
		if code, _, _ := post(t, c, base+"/wp-admin/options-permalink", url.Values{"option_page": {"permalink"}, "_wpnonce": {formNonce(t, form)}, "selection": {structure}}); code != http.StatusFound {
			t.Fatalf("permalink settings: %d", code)
		}
	}
	setPermalinks("/%postname%/")
	permalink := base + "/embed-me/"

	// JSON response.
	code, body, _ := get(t, anon, base+"/wp-json/oembed/1.0/embed?url="+url.QueryEscape(permalink)+"&maxwidth=400")
	if code != http.StatusOK {
		t.Fatalf("oembed: %d %s", code, body)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"version": "1.0", "provider_name": "Embed Site", "provider_url": base, "author_name": "admin",
		"title": "Embed me", "type": "rich", "width": float64(400), "height": float64(225)} {
		if data[k] != want {
			t.Errorf("oembed %s = %v, want %v", k, data[k], want)
		}
	}
	if au, _ := data["author_url"].(string); au != base+"/author/admin/" {
		t.Errorf("author_url = %q", data["author_url"])
	}
	htmlOut, _ := data["html"].(string)
	for _, want := range []string{`<blockquote class="wp-embedded-content" data-secret="`, `<a href="` + permalink + `">Embed me</a></blockquote>`,
		`<iframe sandbox="allow-scripts" security="restricted" style="position: absolute; visibility: hidden;" src="` + permalink + `embed/#?secret=`,
		`width="400" height="225" title="&#8220;Embed me&#8221; &#8212; Embed Site"`, `class="wp-embedded-content"></iframe>`, `<script type="text/javascript">`, "receiveEmbedMessage"} {
		if !strings.Contains(htmlOut, want) {
			t.Errorf("oembed html misses %q in %s", want, htmlOut)
		}
	}
	if strings.Contains(body, "_links") {
		t.Error("oembed response must not carry _links")
	}
	// maxwidth is clamped to WordPress' bounds, the default is 600.
	_, body, _ = get(t, anon, base+"/wp-json/oembed/1.0/embed?url="+url.QueryEscape(permalink))
	if !strings.Contains(body, `"width":600`) || !strings.Contains(body, `"height":338`) {
		t.Errorf("default size: %s", body)
	}
	_, body, _ = get(t, anon, base+"/wp-json/oembed/1.0/embed?url="+url.QueryEscape(permalink)+"&maxwidth=50")
	if !strings.Contains(body, `"width":200`) {
		t.Errorf("minimum width: %s", body)
	}

	// XML response.
	resp, err := anon.Get(base + "/wp-json/oembed/1.0/embed?url=" + url.QueryEscape(permalink) + "&format=xml")
	if err != nil {
		t.Fatal(err)
	}
	xmlBody := make([]byte, 4096)
	n, _ := resp.Body.Read(xmlBody)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/xml") {
		t.Errorf("xml content type = %q", ct)
	}
	if x := string(xmlBody[:n]); !strings.HasPrefix(x, `<?xml version="1.0" encoding="utf-8" standalone="yes"?><oembed><version>1.0</version><provider_name>Embed Site</provider_name>`) || !strings.Contains(x, "<title>Embed me</title><type>rich</type>") {
		t.Errorf("xml: %s", x)
	}

	// Errors: missing url, bad format, unknown URL, foreign host, draft post.
	if code, body, _ := get(t, anon, base+"/wp-json/oembed/1.0/embed"); code != http.StatusBadRequest || !strings.Contains(body, `"rest_missing_callback_param"`) || !strings.Contains(body, "Missing parameter(s): url") {
		t.Errorf("missing url: %d %s", code, body)
	}
	if code, body, _ := get(t, anon, base+"/wp-json/oembed/1.0/embed?url="+url.QueryEscape(permalink)+"&format=yaml"); code != http.StatusBadRequest || !strings.Contains(body, `"rest_invalid_param"`) {
		t.Errorf("bad format: %d %s", code, body)
	}
	for _, bad := range []string{base + "/?p=99999", "https://other.example/?p=" + itoa64(id), base + "/no-such-page/"} {
		if code, body, _ := get(t, anon, base+"/wp-json/oembed/1.0/embed?url="+url.QueryEscape(bad)); code != http.StatusNotFound || !strings.Contains(body, `"oembed_invalid_url"`) || !strings.Contains(body, `"Not Found"`) {
			t.Errorf("unknown url %s: %d %s", bad, code, body)
		}
	}
	var draft int64
	_ = pool.QueryRow(ctx, `INSERT INTO posts (type, author_id, slug, title, status) VALUES ('post', 1, 'secret-draft', 'Secret', 'draft') RETURNING id`).Scan(&draft)
	if code, _, _ := get(t, anon, base+"/wp-json/oembed/1.0/embed?url="+url.QueryEscape(base+"/?p="+itoa64(draft))); code != http.StatusNotFound {
		t.Errorf("draft embeddable: %d", code)
	}

	// Pretty permalinks (Settings › Permalinks): the embed URL becomes /<slug>/embed/.
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	_, form, _ := get(t, c, base+"/wp-admin/options-permalink")
	if code, _, _ := post(t, c, base+"/wp-admin/options-permalink", url.Values{"option_page": {"permalink"}, "_wpnonce": {formNonce(t, form)}, "selection": {"/%postname%/"}}); code != http.StatusFound {
		t.Fatalf("permalink settings: %d", code)
	}
	permalink = base + "/embed-me/"
	_, body, _ = get(t, anon, base+"/wp-json/oembed/1.0/embed?url="+url.QueryEscape(permalink))
	if !strings.Contains(body, `src=\"`+permalink+`embed/#?secret=`) {
		t.Errorf("pretty embed src: %s", body)
	}

	// The embed template (?embed=true and /embed/).
	for _, u := range []string{permalink + "?embed=true", permalink + "embed/"} {
		resp, err := anon.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64<<10)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		page := string(buf[:n])
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-WP-embed") != "true" {
			t.Fatalf("embed %s: %d X-WP-embed=%q", u, resp.StatusCode, resp.Header.Get("X-WP-embed"))
		}
		for _, want := range []string{`<html lang="en-US" class="no-js">`, "<title>Embed me &#8211; Embed Site</title>", `<body class="wp-embed-responsive">`,
			`<div class="wp-embed post-` + itoa64(id) + ` post type-post status-publish hentry format-standard">`, `<p class="wp-embed-heading">`,
			`<a href="` + permalink + `" target="_top">`, `<div class="wp-embed-excerpt"><p>First words of the post.`, `<div class="wp-embed-footer">`,
			`<div class="wp-embed-site-title">`, `<span>Embed Site</span>`, `<div class="wp-embed-comments">`, "0 Comments", `class="wp-embed-share-dialog-open"`,
			`<div class="wp-embed-share-dialog hidden"`, "wp-embed-share-input", `<style type="text/css">`, ".wp-embed{", `<script type="text/javascript">`} {
			if !strings.Contains(page, want) {
				t.Errorf("embed template %s misses %q", u, want)
			}
		}
	}
	// An unknown URL renders embed-404.php.
	if code, body, _ := get(t, anon, base+"/nothing-here/embed/"); code != http.StatusNotFound || !strings.Contains(body, "Oops! That embed cannot be found.") || !strings.Contains(body, `<a href="`+base+`/">Embed Site</a> directly?`) {
		t.Errorf("embed 404: %d", code)
	}

	// Discovery links on the single view only.
	_, body, _ = get(t, anon, permalink)
	disc := `<link rel="alternate" title="oEmbed (JSON)" type="application/json+oembed" href="` + base + `/wp-json/oembed/1.0/embed?url=` + url.QueryEscape(permalink) + `" />`
	if !strings.Contains(body, disc) || !strings.Contains(body, `type="text/xml+oembed"`) || !strings.Contains(body, "&#038;format=xml") {
		t.Errorf("single view lacks the oEmbed discovery links")
	}
	if _, body, _ := get(t, anon, base+"/"); strings.Contains(body, "json+oembed") {
		t.Errorf("home view has oEmbed discovery links")
	}

	// Plain permalinks: the embed URL is the query-var form, ?embed=true.
	setPermalinks("")
	plain := base + "/?p=" + itoa64(id)
	_, body, _ = get(t, anon, base+"/wp-json/oembed/1.0/embed?url="+url.QueryEscape(plain))
	var plainData map[string]any
	_ = json.Unmarshal([]byte(body), &plainData)
	if h, _ := plainData["html"].(string); !strings.Contains(h, `src="`+plain+`&amp;embed=true#?secret=`) {
		t.Errorf("plain embed src: %s", h)
	}
	if code, body, _ := get(t, anon, plain+"&embed=true"); code != http.StatusOK || !strings.Contains(body, `<p class="wp-embed-heading">`) {
		t.Errorf("plain embed template: %d", code)
	}
}
