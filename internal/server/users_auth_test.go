package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"wpgo/internal/mail"
)

// sentMail reads the e-mails queued by sendMail (the wp_mail jobs), oldest first.
func sentMail(t *testing.T) []mail.Message {
	t.Helper()
	rows, err := getPool(t).Query(context.Background(), `SELECT payload FROM jobs WHERE name = 'wp_mail' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []mail.Message
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m mail.Message
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out
}

// lastMail returns the newest e-mail whose subject contains subject.
func lastMail(t *testing.T, subject string) mail.Message {
	t.Helper()
	all := sentMail(t)
	for i := len(all) - 1; i >= 0; i-- {
		if strings.Contains(all[i].Subject, subject) {
			return all[i]
		}
	}
	t.Fatalf("no e-mail with subject %q among %d messages", subject, len(all))
	return mail.Message{}
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"]+`)

func firstURL(t *testing.T, body, contains string) string {
	t.Helper()
	for _, u := range urlRe.FindAllString(body, -1) {
		if strings.Contains(u, contains) {
			return u
		}
	}
	t.Fatalf("no URL containing %q in %q", contains, body)
	return ""
}

func installUsersSite(t *testing.T, c *http.Client, base string) {
	t.Helper()
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Users Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK || !strings.Contains(body, "Success!") {
		t.Fatalf("install: %d", code)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
}

type restClient struct {
	t     *testing.T
	c     *http.Client
	base  string
	nonce string
	basic [2]string
}

func newRESTClient(t *testing.T, c *http.Client, base string) *restClient {
	t.Helper()
	_, nonce, _ := get(t, c, base+"/wp-admin/admin-ajax?action=rest-nonce")
	return &restClient{t: t, c: c, base: base, nonce: nonce}
}

func (r *restClient) do(method, path string, payload any) (int, map[string]any, []any, http.Header) {
	r.t.Helper()
	var rd io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.base+"/wp-json"+path, rd)
	if r.basic[0] != "" {
		req.SetBasicAuth(r.basic[0], r.basic[1])
	} else if r.nonce != "" {
		req.Header.Set("X-WP-Nonce", r.nonce)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		r.t.Fatalf("%s %s: bad JSON %q", method, path, raw)
	}
	obj, _ := out.(map[string]any)
	list, _ := out.([]any)
	return resp.StatusCode, obj, list, resp.Header
}

func TestUserRegistrationAndEmailConfirmations(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installUsersSite(t, c, base)
	anon := &http.Client{CheckRedirect: c.CheckRedirect}

	// Registration is off by default: wp-login.php?action=register redirects to ?registration=disabled.
	if code, _, loc := get(t, anon, base+"/wp-login?action=register"); code != http.StatusFound || loc != "/wp-login?registration=disabled" {
		t.Fatalf("register while disabled: %d %s", code, loc)
	}
	if _, body, _ := get(t, anon, base+"/wp-login?registration=disabled"); !strings.Contains(body, "User registration is currently not allowed.") {
		t.Error("registration=disabled message missing")
	}
	if _, body, _ := get(t, anon, base+"/wp-login"); strings.Contains(body, "wp-login-register") {
		t.Error("Register link shown while registration is off")
	}
	if err := srv.settings.Set(context.Background(), map[string]any{"users_can_register": true, "default_role": "contributor"}); err != nil {
		t.Fatal(err)
	}
	_, body, _ := get(t, anon, base+"/wp-login")
	if !strings.Contains(body, `<a class="wp-login-register" href="/wp-login?action=register">Register</a> | <a class="wp-login-lost-password"`) {
		t.Error("login form lacks the Register link")
	}
	_, body, _ = get(t, anon, base+"/wp-login?action=register")
	for _, want := range []string{`id="registerform" action="/wp-login?action=register"`, `name="user_login"`, `name="user_email"`, "Registration confirmation will be emailed to you.", `value="Register"`, "Register For This Site"} {
		if !strings.Contains(body, want) {
			t.Errorf("registration form misses %q", want)
		}
	}
	// Validation messages are WordPress'.
	_, body, _ = post(t, anon, base+"/wp-login?action=register", url.Values{"user_login": {""}, "user_email": {"bad"}})
	if !strings.Contains(body, "Please enter a username.") || !strings.Contains(body, "The email address isn&#8217;t correct.") {
		t.Errorf("registration validation: %s", body)
	}
	_, body, _ = post(t, anon, base+"/wp-login?action=register", url.Values{"user_login": {"admin"}, "user_email": {"admin@example.test"}})
	if !strings.Contains(body, "This username is already registered.") || !strings.Contains(body, "This email address is already registered.") {
		t.Errorf("duplicate registration: %s", body)
	}
	code, _, loc := post(t, anon, base+"/wp-login?action=register", url.Values{"user_login": {"newbie"}, "user_email": {"newbie@example.test"}})
	if code != http.StatusFound || loc != "/wp-login?checkemail=registered" {
		t.Fatalf("register: %d %s", code, loc)
	}
	if _, body, _ := get(t, anon, base+loc); !strings.Contains(body, "Registration complete. Please check your email, then visit the login page.") {
		t.Error("checkemail=registered message missing")
	}
	var role string
	_ = getPool(t).QueryRow(context.Background(), `SELECT r.slug FROM users u JOIN user_roles ur ON ur.user_id = u.id JOIN roles r ON r.id = ur.role_id WHERE u.login = 'newbie'`).Scan(&role)
	if role != "contributor" {
		t.Errorf("new user role = %q, want the default_role contributor", role)
	}
	adminMail := lastMail(t, "[Users Site] New User Registration")
	if adminMail.To[0] != "admin@example.test" || !strings.Contains(adminMail.Body, "Username: newbie") {
		t.Errorf("admin notification: %+v", adminMail)
	}
	userMail := lastMail(t, "[Users Site] Login Details")
	if userMail.To[0] != "newbie@example.test" || !strings.Contains(userMail.Body, "To set your password, visit the following address:") {
		t.Errorf("user notification: %+v", userMail)
	}
	// The set-password link from the notification works.
	rp := firstURL(t, userMail.Body, "action=rp")
	if code, body, _ := get(t, anon, rp); code != http.StatusOK || !strings.Contains(body, `id="resetpassform"`) {
		t.Errorf("set-password link: %d", code)
	}

	// Profile e-mail change: the old address stays until the link in "[Site] Email Change Request" is followed.
	_, body, _ = get(t, c, base+"/wp-admin/profile")
	nonce := formNonce(t, body)
	form := url.Values{"_wpnonce": {nonce}, "nickname": {"admin"}, "email": {"new-admin@example.test"}, "display_name": {"admin"}, "admin_color": {"fresh"}}
	if code, _, loc := post(t, c, base+"/wp-admin/profile", form); code != http.StatusFound || loc != "/wp-admin/profile?updated=1" {
		t.Fatalf("profile save: %d %s", code, loc)
	}
	var email string
	_ = getPool(t).QueryRow(context.Background(), `SELECT email FROM users WHERE login = 'admin'`).Scan(&email)
	if email != "admin@example.test" {
		t.Errorf("email changed without confirmation: %q", email)
	}
	_, body, _ = get(t, c, base+"/wp-admin/profile?updated=1")
	if !strings.Contains(body, "Your email address has not been updated yet. Please check your inbox at <code>new-admin@example.test</code>") ||
		!strings.Contains(body, "There is a pending change of your email to <code>new-admin@example.test</code>.") || !strings.Contains(body, "dismiss=") {
		t.Error("pending e-mail change notices missing")
	}
	change := lastMail(t, "[Users Site] Email Change Request")
	if change.To[0] != "new-admin@example.test" || !strings.Contains(change.Body, "Howdy admin,") || !strings.Contains(change.Body, "This email has been sent to new-admin@example.test") {
		t.Errorf("email change mail: %+v", change)
	}
	confirm := firstURL(t, change.Body, "newuseremail=")
	if code, _, loc := get(t, c, confirm); code != http.StatusFound || loc != "/wp-admin/profile?updated=1" {
		t.Fatalf("confirm e-mail: %d %s", code, loc)
	}
	_ = getPool(t).QueryRow(context.Background(), `SELECT email FROM users WHERE login = 'admin'`).Scan(&email)
	if email != "new-admin@example.test" {
		t.Errorf("email after confirmation = %q", email)
	}
	if code, _, loc := get(t, c, base+"/wp-admin/profile?newuseremail=nope"); code != http.StatusFound || loc != "/wp-admin/profile?error=new-email" {
		t.Errorf("bad hash: %d %s", code, loc)
	}
	if _, body, _ := get(t, c, base+"/wp-admin/profile?error=new-email"); !strings.Contains(body, "Error while saving the new email address. Please try again.") {
		t.Error("new-email error notice missing")
	}
	// A pending change can be cancelled.
	_, body, _ = get(t, c, base+"/wp-admin/profile")
	form.Set("_wpnonce", formNonce(t, body))
	form.Set("email", "third@example.test")
	post(t, c, base+"/wp-admin/profile", form)
	_, body, _ = get(t, c, base+"/wp-admin/profile")
	dismiss := regexp.MustCompile(`href="(/wp-admin/profile\?dismiss=[^"]+)"`).FindStringSubmatch(body)
	if dismiss == nil {
		t.Fatal("no Cancel link for the pending change")
	}
	if code, _, _ := get(t, c, base+strings.ReplaceAll(dismiss[1], "&amp;", "&")); code != http.StatusFound {
		t.Errorf("dismiss: %d", code)
	}
	if _, body, _ := get(t, c, base+"/wp-admin/profile"); strings.Contains(body, "There is a pending change of your email") {
		t.Error("pending change still shown after Cancel")
	}

	// Settings › General: new_admin_email + the options.php?adminhash= confirmation.
	_, body, _ = get(t, c, base+"/wp-admin/options-general")
	if !strings.Contains(body, `name="new_admin_email"`) {
		t.Error("general settings field is not new_admin_email")
	}
	general := url.Values{"option_page": {"general"}, "_wpnonce": {formNonce(t, body)}, "blogname": {"Users Site"}, "new_admin_email": {"owner@example.test"},
		"timezone_string": {"UTC"}, "date_format": {"F j, Y"}, "time_format": {"g:i a"}, "start_of_week": {"1"}, "default_role": {"subscriber"}}
	if code, _, _ := post(t, c, base+"/wp-admin/options", general); code != http.StatusFound {
		t.Fatalf("general settings: %d", code)
	}
	if srv.settings.String("admin_email") != "admin@example.test" {
		t.Errorf("admin_email changed without confirmation: %q", srv.settings.String("admin_email"))
	}
	if _, body, _ := get(t, c, base+"/wp-admin/options-general"); !strings.Contains(body, "There is a pending change of the admin email to <code>owner@example.test</code>.") {
		t.Error("pending admin e-mail notice missing")
	}
	adminChange := lastMail(t, "[Users Site] New Admin Email Address")
	if adminChange.To[0] != "owner@example.test" || !strings.Contains(adminChange.Body, "administration email address changed on this site:") {
		t.Errorf("admin email mail: %+v", adminChange)
	}
	if code, _, loc := get(t, c, firstURL(t, adminChange.Body, "adminhash=")); code != http.StatusFound || loc != "/wp-admin/options-general?updated=true" {
		t.Errorf("adminhash: %d %s", code, loc)
	}
	if srv.settings.String("admin_email") != "owner@example.test" || srv.settings.String("new_admin_email") != "" {
		t.Errorf("admin_email after confirmation = %q", srv.settings.String("admin_email"))
	}

	// Users list: "Send password reset" row action and bulk action.
	_, body, _ = get(t, c, base+"/wp-admin/users")
	reset := regexp.MustCompile(`href="(/wp-admin/users\?action=resetpassword&amp;users=\d+&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	if reset == nil || !strings.Contains(body, `<option value="resetpassword">Send password reset</option>`) {
		t.Fatal("users list lacks the Send password reset actions")
	}
	if !strings.Contains(body, `column-username column-primary sorted asc"`) || !strings.Contains(body, `column-email sortable`) {
		t.Error("users list has no sortable Username / Email headers")
	}
	if code, _, loc := get(t, c, base+strings.ReplaceAll(reset[1], "&amp;", "&")); code != http.StatusFound || loc != "/wp-admin/users?update=resetpassword&reset_count=1" {
		t.Errorf("row reset: %d %s", code, loc)
	}
	if _, body, _ := get(t, c, base+"/wp-admin/users?update=resetpassword&reset_count=1"); !strings.Contains(body, "Password reset link sent to 1 user.") {
		t.Error("reset notice missing")
	}
	m := lastMail(t, "[Users Site] Password Reset")
	if m.To[0] != "newbie@example.test" || !strings.Contains(m.Body, "Username: newbie") {
		t.Errorf("reset mail: %+v", m)
	}
	var newbieID int64
	_ = getPool(t).QueryRow(context.Background(), `SELECT id FROM users WHERE login = 'newbie'`).Scan(&newbieID)
	bulk := url.Values{"_wpnonce": {formNonce(t, body)}, "action": {"-1"}, "action2": {"resetpassword"}, "users[]": {itoa64(newbieID)}}
	if code, _, loc := get(t, c, base+"/wp-admin/users?"+bulk.Encode()); code != http.StatusFound || loc != "/wp-admin/users?update=resetpassword&reset_count=1" {
		t.Errorf("bulk reset: %d %s", code, loc)
	}
}

func TestApplicationPasswords(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	base := ts.URL
	installUsersSite(t, c, base)

	_, body, _ := get(t, c, base+"/wp-admin/profile")
	for _, want := range []string{`id="application-passwords-section"`, "Add New Application Password", `name="new_application_password_name"`, `class="wp-list-table widefat fixed striped table-view-list application-passwords-user"`} {
		if !strings.Contains(body, want) {
			t.Errorf("profile misses %q", want)
		}
	}
	nonce := formNonce(t, body)
	_, body, _ = post(t, c, base+"/wp-admin/profile", url.Values{"_wpnonce": {nonce}, "do_new_application_password": {"1"}, "new_application_password_name": {"Mobile App"}})
	shown := regexp.MustCompile(`id="new-application-password-value" type="text" class="code" readonly="readonly" value="([^"]+)"`).FindStringSubmatch(body)
	if shown == nil || !regexp.MustCompile(`^[A-Za-z0-9]{4}( [A-Za-z0-9]{4}){5}$`).MatchString(shown[1]) {
		t.Fatalf("new password not shown in xxxx xxxx format: %v", shown)
	}
	if !strings.Contains(body, "Your new password for <strong>Mobile App</strong> is:") || !strings.Contains(body, `<td class="name column-name has-row-actions column-primary" data-colname="Name">Mobile App</td>`) {
		t.Error("new password notice or table row missing")
	}
	var hash string
	_ = getPool(t).QueryRow(context.Background(), `SELECT password_hash FROM app_passwords WHERE name = 'Mobile App'`).Scan(&hash)
	if hash == "" || strings.Contains(hash, strings.ReplaceAll(shown[1], " ", "")) {
		t.Error("application password is not stored hashed")
	}
	_, body, _ = post(t, c, base+"/wp-admin/profile", url.Values{"_wpnonce": {nonce}, "do_new_application_password": {"1"}, "new_application_password_name": {"mobile app"}})
	if !strings.Contains(body, "Each application name should be unique.") {
		t.Error("duplicate name accepted")
	}

	// The password authenticates REST requests (spaces ignored), updates last used / IP and introspects.
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	rc := &restClient{t: t, c: anon, base: base, basic: [2]string{"admin", shown[1]}}
	code, me, _, _ := rc.do("GET", "/wp/v2/users/me?context=edit", nil)
	if code != http.StatusOK || me["username"] != "admin" {
		t.Fatalf("basic auth with application password: %d %v", code, me)
	}
	code, intro, _, _ := rc.do("GET", "/wp/v2/users/me/application-passwords/introspect", nil)
	if code != http.StatusOK || intro["name"] != "Mobile App" || intro["last_ip"] == nil {
		t.Errorf("introspect: %d %v", code, intro)
	}
	rc.basic = [2]string{"admin", "wrong"}
	if code, _, _, _ := rc.do("GET", "/wp/v2/users/me", nil); code != http.StatusUnauthorized {
		t.Errorf("wrong application password: %d", code)
	}
	if code, _, _, _ := (&restClient{t: t, c: anon, base: base}).do("GET", "/wp/v2/users/me/application-passwords", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous list: %d", code)
	}

	// REST controller: create (password returned once), list, read, rename, delete, delete all.
	rest := newRESTClient(t, c, base)
	code, created, _, hdr := rest.do("POST", "/wp/v2/users/me/application-passwords", map[string]any{"name": "Zapier", "app_id": "123e4567-e89b-42d3-a456-426614174000"})
	if code != http.StatusCreated || created["name"] != "Zapier" || created["app_id"] != "123e4567-e89b-42d3-a456-426614174000" || created["last_used"] != nil {
		t.Fatalf("create: %d %v", code, created)
	}
	pw, _ := created["password"].(string)
	uuid, _ := created["uuid"].(string)
	if len(pw) != 24 || !isUUID(uuid) || !strings.HasSuffix(hdr.Get("Location"), "/wp-json/wp/v2/users/1/application-passwords/"+uuid) {
		t.Errorf("create response: %v %s", created, hdr.Get("Location"))
	}
	if self, _ := created["_links"].(map[string]any)["self"].([]any); len(self) != 1 {
		t.Errorf("_links: %v", created["_links"])
	}
	code, dup, _, _ := rest.do("POST", "/wp/v2/users/1/application-passwords", map[string]any{"name": "Zapier"})
	if code != http.StatusBadRequest || dup["code"] != "application_password_duplicate_name" {
		t.Errorf("duplicate: %d %v", code, dup)
	}
	code, missing, _, _ := rest.do("POST", "/wp/v2/users/1/application-passwords", map[string]any{})
	if code != http.StatusBadRequest || missing["code"] != "rest_missing_callback_param" {
		t.Errorf("missing name: %d %v", code, missing)
	}
	code, _, list, _ := rest.do("GET", "/wp/v2/users/1/application-passwords", nil)
	if code != http.StatusOK || len(list) != 2 {
		t.Errorf("list: %d %d", code, len(list))
	}
	if item, _ := list[0].(map[string]any); item["password"] != nil {
		t.Error("list leaks passwords")
	}
	code, one, _, _ := rest.do("GET", "/wp/v2/users/me/application-passwords/"+uuid, nil)
	if code != http.StatusOK || one["uuid"] != uuid {
		t.Errorf("get: %d %v", code, one)
	}
	code, renamed, _, _ := rest.do("PUT", "/wp/v2/users/me/application-passwords/"+uuid, map[string]any{"name": "Zapier 2"})
	if code != http.StatusOK || renamed["name"] != "Zapier 2" {
		t.Errorf("rename: %d %v", code, renamed)
	}
	code, del, _, _ := rest.do("DELETE", "/wp/v2/users/me/application-passwords/"+uuid, nil)
	if code != http.StatusOK || del["deleted"] != true || del["previous"].(map[string]any)["uuid"] != uuid {
		t.Errorf("delete: %d %v", code, del)
	}
	code, nf, _, _ := rest.do("GET", "/wp/v2/users/me/application-passwords/"+uuid, nil)
	if code != http.StatusNotFound || nf["code"] != "rest_application_password_not_found" {
		t.Errorf("deleted lookup: %d %v", code, nf)
	}
	code, all, _, _ := rest.do("DELETE", "/wp/v2/users/1/application-passwords", nil)
	if code != http.StatusOK || all["deleted"] != true || all["count"] != float64(1) {
		t.Errorf("delete all: %d %v", code, all)
	}

	// authorize-application.php: the approval form, approve with success_url, reject, and the inline password.
	_, body, _ = get(t, c, base+"/wp-admin/authorize-application?app_name=Desk+Client&success_url=https%3A%2F%2Fapp.example%2Fcb%3Fx%3D1&reject_url=https%3A%2F%2Fapp.example%2Fno")
	for _, want := range []string{"<h1>Authorize Application</h1>", "An application would like to connect to your account.", "identifying itself as <strong>Desk Client</strong>",
		`name="action" value="authorize_application_password"`, `name="success_url" value="https://app.example/cb?x=1"`, "Yes, I approve of this connection", "No, I do not approve of this connection",
		"You will be sent to <strong>https://app.example/cb?x=1</strong>"} {
		if !strings.Contains(body, want) {
			t.Errorf("authorize form misses %q", want)
		}
	}
	nonce = formNonce(t, body)
	code, _, loc := post(t, c, base+"/wp-admin/authorize-application", url.Values{"_wpnonce": {nonce}, "action": {"authorize_application_password"}, "app_name": {"Desk Client"},
		"success_url": {"https://app.example/cb?x=1"}, "reject_url": {"https://app.example/no"}, "approve": {"1"}})
	if code != http.StatusFound || !strings.HasPrefix(loc, "https://app.example/cb?x=1&site_url=") {
		t.Fatalf("approve: %d %s", code, loc)
	}
	q, _ := url.Parse(loc)
	if q.Query().Get("user_login") != "admin" || len(q.Query().Get("password")) != 24 || !strings.HasPrefix(q.Query().Get("site_url"), "http://") {
		t.Errorf("success redirect params: %s", loc)
	}
	rc.basic = [2]string{"admin", q.Query().Get("password")}
	if code, _, _, _ := rc.do("GET", "/wp/v2/users/me", nil); code != http.StatusOK {
		t.Errorf("password from authorize flow rejected: %d", code)
	}
	if code, _, loc := post(t, c, base+"/wp-admin/authorize-application", url.Values{"_wpnonce": {nonce}, "action": {"authorize_application_password"}, "app_name": {"Desk Client"},
		"reject_url": {"https://app.example/no"}, "reject": {"1"}}); code != http.StatusFound || loc != "https://app.example/no" {
		t.Errorf("reject: %d %s", code, loc)
	}
	_, body, _ = post(t, c, base+"/wp-admin/authorize-application", url.Values{"_wpnonce": {nonce}, "action": {"authorize_application_password"}, "app_name": {"Manual"}, "approve": {"1"}})
	if !regexp.MustCompile(`value="[A-Za-z0-9]{4}( [A-Za-z0-9]{4}){5}"`).MatchString(body) || !strings.Contains(body, "Your new password for <strong>Manual</strong> is:") {
		t.Error("approve without success_url does not show the password")
	}
	if code, body, _ := get(t, c, base+"/wp-admin/authorize-application?app_name=X&success_url=http%3A%2F%2Fevil.example%2F"); code != http.StatusBadRequest || !strings.Contains(body, "The URL must be served over a secure connection.") {
		t.Errorf("insecure success_url accepted: %d", code)
	}
	_, body, _ = get(t, c, base+"/wp-json/")
	if !strings.Contains(body, `"authorization":"`) || !strings.Contains(body, "/wp-admin/authorize-application") {
		t.Error("REST index does not advertise authorize-application")
	}
}

func TestPrivacyRequests(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installUsersSite(t, c, base)
	anon := &http.Client{CheckRedirect: c.CheckRedirect}
	pool := getPool(t)
	ctx := context.Background()

	// A comment by the person, so the export and the erasure have something to work on.
	var postID int64
	_ = pool.QueryRow(ctx, `SELECT id FROM posts WHERE type = 'post' AND status = 'publish' ORDER BY id LIMIT 1`).Scan(&postID)
	if _, err := pool.Exec(ctx, `INSERT INTO comments(post_id, author_name, author_email, author_url, author_ip, user_agent, content, status)
		VALUES ($1, 'Pat Person', 'pat@example.test', 'https://pat.example', '203.0.113.77', 'UA/1', 'My comment', 'approved')`, postID); err != nil {
		t.Fatal(err)
	}

	// Export request with the confirmation e-mail.
	_, body, _ := get(t, c, base+"/wp-admin/export-personal-data")
	if !strings.Contains(body, "Send personal data export confirmation email.") {
		t.Error("confirmation checkbox missing")
	}
	nonce := formNonce(t, body)
	_, body, _ = post(t, c, base+"/wp-admin/export-personal-data", url.Values{"_wpnonce": {nonce}, "action": {"add_export_personal_data_request"},
		"username_or_email_for_privacy_request": {"pat@example.test"}, "send_confirmation_email": {"1"}})
	for _, want := range []string{"Confirmation request initiated successfully.", `class="status-label status-request-pending">Pending</span>`, "Waiting for confirmation", "Resend confirmation request", "Mark as completed", "Remove request", "Download personal data"} {
		if !strings.Contains(body, want) {
			t.Errorf("export screen misses %q", want)
		}
	}
	_, body, _ = post(t, c, base+"/wp-admin/export-personal-data", url.Values{"_wpnonce": {nonce}, "action": {"add_export_personal_data_request"},
		"username_or_email_for_privacy_request": {"pat@example.test"}, "send_confirmation_email": {"1"}})
	if !strings.Contains(body, "An incomplete personal data request for this email address already exists.") {
		t.Error("duplicate request accepted")
	}
	_, body, _ = post(t, c, base+"/wp-admin/export-personal-data", url.Values{"_wpnonce": {nonce}, "action": {"add_export_personal_data_request"}, "username_or_email_for_privacy_request": {"nobody"}})
	if !strings.Contains(body, "Unable to initiate confirmation request. Invalid email address or username.") {
		t.Error("invalid address accepted")
	}
	confirmMail := lastMail(t, "[Users Site] Confirm Action: Export Personal Data")
	if confirmMail.To[0] != "pat@example.test" || !strings.Contains(confirmMail.Body, "     Export Personal Data") {
		t.Errorf("confirmation mail: %+v", confirmMail)
	}
	confirmURL := firstURL(t, confirmMail.Body, "action=confirmaction")
	if code, body, _ := get(t, anon, strings.Replace(confirmURL, "confirm_key=", "confirm_key=x", 1)); code != http.StatusForbidden || !strings.Contains(body, "The confirmation key is invalid for this personal data request.") {
		t.Errorf("bad key: %d", code)
	}
	code, body, _ := get(t, anon, confirmURL)
	if code != http.StatusOK || !strings.Contains(body, "Thanks for confirming your export request.") || !strings.Contains(body, "The site administrator has been notified.") {
		t.Fatalf("confirmaction: %d %s", code, body)
	}
	if code, body, _ := get(t, anon, confirmURL); code != http.StatusForbidden || !strings.Contains(body, "This personal data request has expired.") {
		t.Errorf("second confirmation: %d", code)
	}
	notified := lastMail(t, "[Users Site] Action Confirmed: Export Personal Data")
	if notified.To[0] != "admin@example.test" || !strings.Contains(notified.Body, "User: pat@example.test") || !strings.Contains(notified.Body, "/wp-admin/export-personal-data") {
		t.Errorf("admin notification: %+v", notified)
	}
	_, body, _ = get(t, c, base+"/wp-admin/export-personal-data")
	if !strings.Contains(body, `status-request-confirmed">Confirmed</span>`) || !strings.Contains(body, "Send export link") {
		t.Error("confirmed request not shown with Send export link")
	}
	sendLink := regexp.MustCompile(`href="(/wp-admin/export-personal-data\?action=send-export&amp;request_id=\d+&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	if sendLink == nil {
		t.Fatal("no Send export link")
	}
	_, body, _ = get(t, c, base+strings.ReplaceAll(sendLink[1], "&amp;", "&"))
	if !strings.Contains(body, "Email sent.") || !strings.Contains(body, `status-request-completed">Completed</span>`) {
		t.Errorf("send export link: %s", body)
	}
	exportMail := lastMail(t, "[Users Site] Personal Data Export")
	if exportMail.To[0] != "pat@example.test" || !strings.Contains(exportMail.Body, "Your request for an export of personal data has been completed.") {
		t.Errorf("export mail: %+v", exportMail)
	}
	fileURL := firstURL(t, exportMail.Body, "wp-personal-data-exports/")
	name := fileURL[strings.LastIndex(fileURL, "/")+1:]
	if !strings.HasPrefix(name, "wp-personal-data-file-") || !strings.HasSuffix(name, ".zip") {
		t.Errorf("export file name %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(srv.cfg.UploadsDir, "wp-personal-data-exports", name))
	if err != nil {
		t.Fatalf("export file: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	var index string
	for _, f := range zr.File {
		if f.Name == "index.html" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			index = string(b)
		}
	}
	for _, want := range []string{"<h1 id=\"top\">Personal Data Export for pat@example.test</h1>", "Table of Contents", `<h2 id="about">About</h2>`, `<h2 id="comments">Comments</h2>`,
		"<th>Comment Author Email</th>\n<td>pat@example.test</td>", "<th>Comment Author IP</th>\n<td>203.0.113.77</td>", "Report generated for"} {
		if !strings.Contains(index, want) {
			t.Errorf("export index.html misses %q", want)
		}
	}
	if code, body, _ := get(t, anon, fileURL); code != http.StatusOK || !strings.HasPrefix(body, "PK") {
		t.Errorf("export file not downloadable: %d", code)
	}
	// "Download personal data" streams the zip to the administrator.
	dl := regexp.MustCompile(`href="(/wp-admin/export-personal-data\?action=download&amp;request_id=\d+&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	_, body, _ = get(t, c, base+"/wp-admin/export-personal-data")
	dl = regexp.MustCompile(`href="(/wp-admin/export-personal-data\?action=download&amp;request_id=\d+&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	if dl == nil {
		t.Fatal("no Download personal data link")
	}
	if code, body, _ := get(t, c, base+strings.ReplaceAll(dl[1], "&amp;", "&")); code != http.StatusOK || !strings.HasPrefix(body, "PK") {
		t.Errorf("download: %d", code)
	}
	// Remove request.
	rm := regexp.MustCompile(`href="(/wp-admin/export-personal-data\?action=delete&amp;request_id=\d+&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	_, body, _ = get(t, c, base+"/wp-admin/export-personal-data")
	rm = regexp.MustCompile(`href="(/wp-admin/export-personal-data\?action=delete&amp;request_id=\d+&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	if rm == nil {
		t.Fatal("no Remove request link")
	}
	if _, body, _ := get(t, c, base+strings.ReplaceAll(rm[1], "&amp;", "&")); !strings.Contains(body, "1 request deleted successfully.") || !strings.Contains(body, "No items found.") {
		t.Error("remove request failed")
	}

	// Erasure request without confirmation e-mail, then erase: the comment is anonymized and the person is told.
	_, body, _ = get(t, c, base+"/wp-admin/erase-personal-data")
	nonce = formNonce(t, body)
	_, body, _ = post(t, c, base+"/wp-admin/erase-personal-data", url.Values{"_wpnonce": {nonce}, "action": {"add_remove_personal_data_request"}, "username_or_email_for_privacy_request": {"pat@example.test"}})
	if !strings.Contains(body, "Request added successfully.") || !strings.Contains(body, `status-request-confirmed">Confirmed</span>`) || !strings.Contains(body, "Erase personal data</a>") {
		t.Errorf("erase request: %s", body)
	}
	erase := regexp.MustCompile(`href="(/wp-admin/erase-personal-data\?action=erase&amp;request_id=\d+&amp;_wpnonce=[^"]+)"`).FindStringSubmatch(body)
	if erase == nil {
		t.Fatal("no Erase personal data link")
	}
	if _, body, _ := get(t, c, base+strings.ReplaceAll(erase[1], "&amp;", "&")); !strings.Contains(body, "Personal data was erased: 1 comments anonymized.") || !strings.Contains(body, `status-request-completed">Completed</span>`) {
		t.Error("erase failed")
	}
	var author, email, ip, ua string
	_ = pool.QueryRow(ctx, `SELECT author_name, author_email, author_ip, user_agent FROM comments WHERE content = 'My comment'`).Scan(&author, &email, &ip, &ua)
	if author != "Anonymous" || email != "deleted@site.invalid" || ip != "203.0.113.0" || ua != "" {
		t.Errorf("comment not anonymized: %s %s %s %q", author, email, ip, ua)
	}
	fulfilled := lastMail(t, "[Users Site] Erasure Request Fulfilled")
	if fulfilled.To[0] != "pat@example.test" || !strings.Contains(fulfilled.Body, "Your request to erase your personal data on Users Site has been completed.") {
		t.Errorf("erasure mail: %+v", fulfilled)
	}

	// Privacy Policy Guide tab.
	_, body, _ = get(t, c, base+"/wp-admin/options-privacy")
	if !strings.Contains(body, `<a href="/wp-admin/options-privacy?tab=policyguide" class="privacy-settings-tab">Policy Guide</a>`) {
		t.Error("Policy Guide tab missing")
	}
	code, body, _ = get(t, c, base+"/wp-admin/options-privacy?tab=policyguide")
	for _, want := range []string{"<h2>Privacy Policy Guide</h2>", "This text template will help you to create your website&#8217;s privacy policy.", "Copy suggested policy text to clipboard",
		`class="privacy-settings-tab active" aria-current="true">Policy Guide</a>`, `<strong class="privacy-policy-tutorial">Suggested text: </strong>`, "Who we are", "Comments", "Media", "Cookies", "Embedded content from other websites",
		"Who we share your data with", "How long we retain your data", "What rights you have over your data", "Where your data is sent"} {
		if !strings.Contains(body, want) {
			t.Errorf("policy guide misses %q", want)
		}
	}
	if code != http.StatusOK || strings.Contains(body, "Template error") {
		t.Fatalf("policy guide: %d", code)
	}
}

func TestRESTCommentsUsersRevisions(t *testing.T) {
	ts, c, _ := testServerBuilderOff(t)
	base := ts.URL
	installUsersSite(t, c, base)
	rest := newRESTClient(t, c, base)
	anon := &restClient{t: t, c: &http.Client{CheckRedirect: c.CheckRedirect}, base: base}
	pool := getPool(t)
	ctx := context.Background()
	var postID int64
	_ = pool.QueryRow(ctx, `SELECT id FROM posts WHERE type = 'post' AND status = 'publish' ORDER BY id LIMIT 1`).Scan(&postID)

	// Comments.
	if code, e, _, _ := anon.do("POST", "/wp/v2/comments", map[string]any{"post": postID, "content": "hi"}); code != http.StatusUnauthorized || e["code"] != "rest_comment_login_required" {
		t.Errorf("anonymous comment: %d %v", code, e)
	}
	if code, e, _, _ := anon.do("GET", "/wp/v2/comments?status=hold", nil); code != http.StatusUnauthorized || e["code"] != "rest_forbidden_param" {
		t.Errorf("anonymous status filter: %d %v", code, e)
	}
	if code, e, _, _ := anon.do("GET", "/wp/v2/comments?context=edit", nil); code != http.StatusUnauthorized || e["code"] != "rest_forbidden_context" {
		t.Errorf("anonymous edit context: %d %v", code, e)
	}
	code, cm, _, hdr := rest.do("POST", "/wp/v2/comments", map[string]any{"post": postID, "content": "First <b>reply</b>"})
	if code != http.StatusCreated || cm["status"] != "approved" || cm["author_email"] != "admin@example.test" || hdr.Get("Location") == "" {
		t.Fatalf("create comment: %d %v", code, cm)
	}
	id := int64(cm["id"].(float64))
	links := cm["_links"].(map[string]any)
	for _, k := range []string{"self", "collection", "up", "author", "children"} {
		if links[k] == nil {
			t.Errorf("comment _links lacks %s", k)
		}
	}
	if code, e, _, _ := rest.do("POST", "/wp/v2/comments", map[string]any{"post": postID, "content": "First <b>reply</b>"}); code != http.StatusConflict || e["code"] != "comment_duplicate" {
		t.Errorf("duplicate comment: %d %v", code, e)
	}
	if code, e, _, _ := rest.do("POST", "/wp/v2/comments", map[string]any{"content": "no post"}); code != http.StatusForbidden || e["code"] != "rest_comment_invalid_post_id" {
		t.Errorf("comment without post: %d %v", code, e)
	}
	code, reply, _, _ := rest.do("POST", "/wp/v2/comments", map[string]any{"post": postID, "parent": id, "content": "Reply", "status": "hold"})
	if code != http.StatusCreated || reply["parent"] != float64(id) || reply["status"] != "hold" {
		t.Errorf("reply: %d %v", code, reply)
	}
	if code, one, _, _ := anon.do("GET", "/wp/v2/comments/"+itoa64(id), nil); code != http.StatusOK || one["author_email"] != nil || one["content"].(map[string]any)["rendered"] == "" {
		t.Errorf("public read: %d %v", code, one)
	}
	if code, e, _, _ := anon.do("GET", "/wp/v2/comments/"+itoa64(int64(reply["id"].(float64))), nil); code != http.StatusUnauthorized || e["code"] != "rest_cannot_read" {
		t.Errorf("public read of a held comment: %d %v", code, e)
	}
	if code, one, _, _ := rest.do("GET", "/wp/v2/comments/"+itoa64(id)+"?context=edit", nil); code != http.StatusOK || one["author_email"] != "admin@example.test" || one["content"].(map[string]any)["raw"] == nil {
		t.Errorf("edit context: %d %v", code, one)
	}
	if code, e, _, _ := rest.do("GET", "/wp/v2/comments/999999", nil); code != http.StatusNotFound || e["code"] != "rest_comment_invalid_id" {
		t.Errorf("missing comment: %d %v", code, e)
	}
	code, _, held, _ := rest.do("GET", "/wp/v2/comments?status=hold&post="+itoa64(postID), nil)
	if code != http.StatusOK || len(held) != 1 {
		t.Errorf("status=hold list: %d %d", code, len(held))
	}
	code, _, children, _ := anon.do("GET", "/wp/v2/comments?parent="+itoa64(id), nil)
	if code != http.StatusOK || len(children) != 0 { // the reply is held, so visitors do not see it
		t.Errorf("children for visitors: %d %d", code, len(children))
	}
	code, upd, _, _ := rest.do("POST", "/wp/v2/comments/"+itoa64(id), map[string]any{"content": "Edited", "status": "hold", "author_name": "Admin Edited"})
	if code != http.StatusOK || upd["status"] != "hold" || upd["author_name"] != "Admin Edited" || upd["content"].(map[string]any)["raw"] != "Edited" {
		t.Errorf("update: %d %v", code, upd)
	}
	if code, e, _, _ := rest.do("POST", "/wp/v2/comments/"+itoa64(id), map[string]any{"type": "pingback"}); code != http.StatusBadRequest || e["code"] != "rest_comment_invalid_type" {
		t.Errorf("type change: %d %v", code, e)
	}
	code, trashed, _, _ := rest.do("DELETE", "/wp/v2/comments/"+itoa64(id), nil)
	if code != http.StatusOK || trashed["status"] != "trash" {
		t.Errorf("trash: %d %v", code, trashed)
	}
	if code, e, _, _ := rest.do("DELETE", "/wp/v2/comments/"+itoa64(id), nil); code != http.StatusGone || e["code"] != "rest_already_trashed" {
		t.Errorf("trash twice: %d %v", code, e)
	}
	code, gone, _, _ := rest.do("DELETE", "/wp/v2/comments/"+itoa64(id)+"?force=true", nil)
	if code != http.StatusOK || gone["deleted"] != true || gone["previous"].(map[string]any)["id"] != float64(id) {
		t.Errorf("force delete: %d %v", code, gone)
	}
	// Password-protected post: comments need the password.
	if _, err := pool.Exec(ctx, `UPDATE posts SET password = 'secret' WHERE id = $1`, postID); err != nil {
		t.Fatal(err)
	}
	if code, e, _, _ := anon.do("GET", "/wp/v2/comments?post="+itoa64(postID), nil); code != http.StatusUnauthorized || e["code"] != "rest_cannot_read_post" {
		t.Errorf("protected post without password: %d %v", code, e)
	}
	if code, _, _, _ := anon.do("GET", "/wp/v2/comments?post="+itoa64(postID)+"&password=secret", nil); code != http.StatusOK {
		t.Errorf("protected post with password: %d", code)
	}
	_, _ = pool.Exec(ctx, `UPDATE posts SET password = '' WHERE id = $1`, postID)

	// Users.
	if code, e, _, _ := anon.do("POST", "/wp/v2/users", map[string]any{"username": "x", "email": "x@example.test", "password": "pw"}); code != http.StatusUnauthorized || e["code"] != "rest_cannot_create_user" {
		t.Errorf("anonymous create: %d %v", code, e)
	}
	if code, e, _, _ := rest.do("POST", "/wp/v2/users", map[string]any{"username": "writer"}); code != http.StatusBadRequest || e["code"] != "rest_missing_callback_param" || e["message"] != "Missing parameter(s): email, password" {
		t.Errorf("missing params: %d %v", code, e)
	}
	code, nu, _, hdr := rest.do("POST", "/wp/v2/users", map[string]any{"username": "writer", "email": "writer@example.test", "password": "pass word", "roles": []string{"author"}, "first_name": "Wri", "name": "Writer W"})
	if code != http.StatusCreated || nu["username"] != "writer" || nu["name"] != "Writer W" || nu["first_name"] != "Wri" || hdr.Get("Location") == "" {
		t.Fatalf("create user: %d %v", code, nu)
	}
	if roles, _ := nu["roles"].([]any); len(roles) != 1 || roles[0] != "author" {
		t.Errorf("roles: %v", nu["roles"])
	}
	uid := itoa64(int64(nu["id"].(float64)))
	if code, e, _, _ := rest.do("POST", "/wp/v2/users", map[string]any{"username": "writer", "email": "other@example.test", "password": "x"}); code != http.StatusBadRequest || e["code"] != "existing_user_login" {
		t.Errorf("duplicate login: %d %v", code, e)
	}
	if code, e, _, _ := rest.do("POST", "/wp/v2/users", map[string]any{"username": "w2", "email": "w2@example.test", "password": "x", "roles": []string{"nope"}}); code != http.StatusBadRequest || e["code"] != "rest_user_invalid_role" {
		t.Errorf("invalid role: %d %v", code, e)
	}
	code, _, authors, _ := rest.do("GET", "/wp/v2/users?roles=author&context=edit", nil)
	if code != http.StatusOK || len(authors) != 1 || authors[0].(map[string]any)["username"] != "writer" {
		t.Errorf("roles filter: %d %v", code, authors)
	}
	code, _, who, _ := rest.do("GET", "/wp/v2/users?who=authors", nil)
	if code != http.StatusOK || len(who) != 2 {
		t.Errorf("who=authors: %d %d", code, len(who))
	}
	code, _, capable, _ := rest.do("GET", "/wp/v2/users?capabilities=manage_options", nil)
	if code != http.StatusOK || len(capable) != 1 {
		t.Errorf("capabilities filter: %d %d", code, len(capable))
	}
	if code, e, _, _ := anon.do("GET", "/wp/v2/users?roles=author", nil); code != http.StatusUnauthorized || e["code"] != "rest_user_cannot_view" {
		t.Errorf("anonymous roles filter: %d %v", code, e)
	}
	if code, _, pub, _ := anon.do("GET", "/wp/v2/users", nil); code != http.StatusOK || len(pub) != 1 { // only admin has published posts
		t.Errorf("public user list: %d %d", code, len(pub))
	}
	if code, e, _, _ := anon.do("GET", "/wp/v2/users/"+uid, nil); code != http.StatusUnauthorized || e["code"] != "rest_user_cannot_view" {
		t.Errorf("public read of a user without posts: %d %v", code, e)
	}
	code, upd2, _, _ := rest.do("POST", "/wp/v2/users/"+uid, map[string]any{"name": "Renamed", "slug": "writer-w", "roles": []string{"editor", "author"}, "description": "bio"})
	if code != http.StatusOK || upd2["name"] != "Renamed" || upd2["slug"] != "writer-w" || upd2["description"] != "bio" {
		t.Errorf("update user: %d %v", code, upd2)
	}
	if roles, _ := upd2["roles"].([]any); len(roles) != 2 || roles[0] != "editor" {
		t.Errorf("updated roles: %v", upd2["roles"])
	}
	if code, e, _, _ := rest.do("POST", "/wp/v2/users/"+uid, map[string]any{"username": "other"}); code != http.StatusBadRequest || e["code"] != "rest_user_invalid_argument" {
		t.Errorf("username edit: %d %v", code, e)
	}
	if code, e, _, _ := rest.do("POST", "/wp/v2/users/me", map[string]any{"roles": []string{"subscriber"}}); code != http.StatusForbidden || e["code"] != "rest_user_invalid_role" {
		t.Errorf("dropping own administrator role: %d %v", code, e)
	}
	if code, e, _, _ := rest.do("DELETE", "/wp/v2/users/"+uid, nil); code != http.StatusNotImplemented || e["code"] != "rest_trash_not_supported" {
		t.Errorf("delete without force: %d %v", code, e)
	}
	if code, e, _, _ := rest.do("DELETE", "/wp/v2/users/"+uid+"?force=true&reassign="+uid, nil); code != http.StatusBadRequest || e["code"] != "rest_user_invalid_reassign" {
		t.Errorf("reassign to self: %d %v", code, e)
	}
	code, deleted, _, _ := rest.do("DELETE", "/wp/v2/users/"+uid+"?force=true&reassign=1", nil)
	if code != http.StatusOK || deleted["deleted"] != true || deleted["previous"].(map[string]any)["username"] != "writer" {
		t.Errorf("delete user: %d %v", code, deleted)
	}

	// Revisions.
	code, p, _, _ := rest.do("POST", "/wp/v2/posts", map[string]any{"title": "Rev test", "content": "v1", "status": "publish"})
	if code != http.StatusCreated {
		t.Fatalf("create post: %d %v", code, p)
	}
	pid := itoa64(int64(p["id"].(float64)))
	if code, _, _, _ := rest.do("POST", "/wp/v2/posts/"+pid, map[string]any{"content": "v2"}); code != http.StatusOK {
		t.Fatalf("update post: %d", code)
	}
	code, _, revs, _ := rest.do("GET", "/wp/v2/posts/"+pid+"/revisions", nil)
	if code != http.StatusOK || len(revs) == 0 {
		t.Fatalf("list revisions: %d %d", code, len(revs))
	}
	rid := itoa64(int64(revs[0].(map[string]any)["id"].(float64)))
	code, rev, _, _ := rest.do("GET", "/wp/v2/posts/"+pid+"/revisions/"+rid, nil)
	if code != http.StatusOK || rev["parent"] != p["id"] || rev["content"].(map[string]any)["raw"] == nil {
		t.Errorf("get revision: %d %v", code, rev)
	}
	if code, e, _, _ := anon.do("GET", "/wp/v2/posts/"+pid+"/revisions/"+rid, nil); code != http.StatusUnauthorized || e["code"] != "rest_cannot_read" {
		t.Errorf("anonymous revision: %d %v", code, e)
	}
	if code, e, _, _ := rest.do("GET", "/wp/v2/pages/"+pid+"/revisions/"+rid, nil); code != http.StatusNotFound || e["code"] != "rest_post_invalid_parent" {
		t.Errorf("wrong parent type: %d %v", code, e)
	}
	if code, e, _, _ := rest.do("DELETE", "/wp/v2/posts/"+pid+"/revisions/"+rid, nil); code != http.StatusNotImplemented || e["code"] != "rest_trash_not_supported" {
		t.Errorf("delete revision without force: %d %v", code, e)
	}
	code, dr, _, _ := rest.do("DELETE", "/wp/v2/posts/"+pid+"/revisions/"+rid+"?force=true", nil)
	if code != http.StatusOK || dr["deleted"] != true {
		t.Errorf("delete revision: %d %v", code, dr)
	}
	if code, e, _, _ := rest.do("GET", "/wp/v2/posts/"+pid+"/revisions/"+rid, nil); code != http.StatusNotFound || e["code"] != "rest_post_invalid_id" {
		t.Errorf("deleted revision: %d %v", code, e)
	}
}
