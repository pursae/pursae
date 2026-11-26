package server

import (
	"html"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"wpgo/internal/store"
)

// A package theme template that prints the author and post objects it can reach shows a visitor no
// password hash, e-mail, user meta, capabilities or post password, on an author archive and on a
// protected single post; bylines and the author archive still render.
func TestThemeTemplatesSeePublicAuthorAndPost(t *testing.T) {
	t.Setenv("PURSAE_THEMES_DIR", t.TempDir())
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Public view"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"owner@example.test"},
	}); code != http.StatusOK {
		t.Fatalf("install: %d %s", code, body)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	ctx := t.Context()
	admin, err := srv.store.UserByLoginOrEmail(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE users SET meta = meta || '{"password_reset":{"hash":"RESETHASH-XYZ"},"secret_note":"META-XYZ"}'::jsonb WHERE id = $1`, admin.ID); err != nil {
		t.Fatal(err)
	}
	mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: admin.ID, Slug: "open-post", Title: "Open post", Content: "<p>Hello</p>"})
	locked := mustInsertPost(t, srv, &store.Post{Type: "post", AuthorID: admin.ID, Slug: "locked-post", Title: "Locked post",
		Content: "<p>Hidden</p>", Password: "POSTPW-XYZ"})

	src := copyThemeTree(t, starterDir)
	footer := filepath.Join(src, "templates", "footer.html")
	b, err := os.ReadFile(footer)
	if err != nil {
		t.Fatal(err)
	}
	probe := `<pre id="probe">{{with .Q.Author}}A={{.DisplayName}}|F={{get_field "favorite_color" .}}|S={{get_field "secret_note" .}}|H={{.PasswordHash}}|E={{.Email}}|M={{json .Meta}}|C={{json .Caps}}|R={{json .Roles}}{{end}}` +
		`{{with .Q.Post}}|QP={{.Password}}{{end}}{{with .Post}}|PP={{with .Post}}{{.Password}}{{end}}|PE={{.AuthorEmail}}{{end}}` +
		`{{range .Posts}}|LP={{.Post.Password}}|LE={{.AuthorEmail}}{{end}}</pre>`
	if err := os.WriteFile(footer, []byte(probe+string(b)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, body, _ := get(t, c, base+"/wp-admin/theme-install?browse=upload")
	if code, _, loc := postZip(t, c, base+"/wp-admin/theme-install", formNonce(t, body), "themezip", "starter.zip", zipDir(t, src)); code != http.StatusFound || strings.Contains(loc, "error=") {
		t.Fatalf("upload: %d %s", code, loc)
	}
	t.Cleanup(func() { unregisterTheme("starter") })
	_, body, _ = get(t, c, base+"/wp-admin/themes")
	m := regexp.MustCompile(`href="(/wp-admin/themes\?action=activate&amp;stylesheet=starter&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no activate link")
	}
	get(t, c, base+html.UnescapeString(m[1]))
	if srv.settings.String("stylesheet") != "starter" {
		t.Fatalf("active theme %q", srv.settings.String("stylesheet"))
	}

	// ACF-Go: a registered user field still renders through the author object; other meta keys do not.
	acfgoActivate(t, c, base)
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Author extras", "location": []any{
		[]any{map[string]any{"param": "user_form", "operator": "==", "value": "all"}}},
		"fields": []any{map[string]any{"key": "field_favcolor", "label": "Favorite color", "name": "favorite_color", "type": "text"}}})
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE users SET meta = meta || '{"favorite_color":"teal-XYZ","_favorite_color":"field_favcolor"}'::jsonb WHERE id = $1`, admin.ID); err != nil {
		t.Fatal(err)
	}

	jar, _ := cookiejar.New(nil)
	visitor := &http.Client{Jar: jar}
	leaks := []string{admin.PasswordHash, "owner@example.test", "RESETHASH-XYZ", "META-XYZ", "POSTPW-XYZ", "administrator"}
	for _, path := range []string{"/?author=" + itoa64(admin.ID), "/author/" + admin.Nicename + "/", "/?p=" + itoa64(locked.ID)} {
		code, page, _ := getPage(t, visitor, base+path)
		if code == http.StatusNotFound && strings.HasPrefix(path, "/?author=") {
			continue // author enumeration by id is refused for visitors; the archive is checked by nicename
		}
		i := strings.Index(page, `<pre id="probe">`)
		if code != http.StatusOK || i < 0 {
			t.Fatalf("%s: %d lacks the probe", path, code)
		}
		frag := html.UnescapeString(page[i : i+strings.Index(page[i:], "</pre>")])
		for _, leak := range leaks {
			if leak != "" && strings.Contains(frag, leak) {
				t.Errorf("%s: a theme template printed %q: %s", path, leak, frag)
			}
		}
		if strings.Contains(path, "author") {
			if !strings.Contains(frag, "F=teal-XYZ") {
				t.Errorf("%s: registered ACF user field lost on the author object: %s", path, frag)
			}
			if !strings.Contains(frag, "A=admin") {
				t.Errorf("%s: author archive lost the author's name: %s", path, frag)
			}
			if !strings.Contains(page, `rel="author">admin</a>`) || !strings.Contains(page, "Open post") {
				t.Errorf("%s: byline or posts missing", path)
			}
		}
	}

	// ACF-Go: get_field on a user returns raw meta only for registered fields.
	for _, key := range []string{"secret_note", "password_reset"} {
		if v := srv.acfGetField(ctx, key, "user_"+itoa64(admin.ID), true); v != nil {
			t.Errorf("get_field %s on a user = %v", key, v)
		}
	}
}
