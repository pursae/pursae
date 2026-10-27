package server

import (
	"crypto/rand"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

// Application Passwords (WP_Application_Passwords): per-user passwords for the REST API, stored hashed
// in app_passwords. The profile screen lists, adds and revokes them; authorize-application.php lets an
// application request one; the REST controller is in rest_app_passwords.go.

type appPassword struct {
	ID        int64
	UUID      string
	AppID     string
	Name      string
	CreatedAt time.Time
	LastUsed  *time.Time
	LastIP    string
}

// appPasswordRow is one row of the profile's application-passwords table.
type appPasswordRow struct {
	appPassword
	Created, Used string
}

// appPasswordCreated is the one-time display of a new password (chunk_password()).
type appPasswordCreated struct {
	Name, Password string
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// isUUID is wp_is_uuid( $uuid, 4 ) relaxed to any RFC 4122 version.
func isUUID(v string) bool { return uuidRe.MatchString(strings.ToLower(v)) }

// newUUID is wp_generate_uuid4().
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

const appPasswordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// generateAppPassword is wp_generate_password( 24, false ).
func generateAppPassword() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = appPasswordAlphabet[int(b[i])%len(appPasswordAlphabet)]
	}
	return string(b)
}

// chunkAppPassword is WP_Application_Passwords::chunk_password(): "xxxx xxxx xxxx xxxx xxxx xxxx".
func chunkAppPassword(raw string) string {
	var parts []string
	for len(raw) > 4 {
		parts = append(parts, raw[:4])
		raw = raw[4:]
	}
	return strings.TrimSpace(strings.Join(append(parts, raw), " "))
}

const appPasswordCols = `id, uuid::text, app_id, name, created_at, last_used_at, last_ip`

func scanAppPassword(row interface{ Scan(dest ...any) error }) (*appPassword, error) {
	ap := &appPassword{}
	if err := row.Scan(&ap.ID, &ap.UUID, &ap.AppID, &ap.Name, &ap.CreatedAt, &ap.LastUsed, &ap.LastIP); err != nil {
		return nil, err
	}
	return ap, nil
}

// appPasswords is WP_Application_Passwords::get_user_application_passwords().
func (s *Server) appPasswords(c *gin.Context, userID int64) ([]*appPassword, error) {
	rows, err := s.store.Pool.Query(c, `SELECT `+appPasswordCols+` FROM app_passwords WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*appPassword
	for rows.Next() {
		ap, err := scanAppPassword(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

func (s *Server) appPasswordByUUID(c *gin.Context, userID int64, uuid string) (*appPassword, error) {
	if !isUUID(uuid) {
		return nil, store.ErrNotFound
	}
	ap, err := scanAppPassword(s.store.Pool.QueryRow(c, `SELECT `+appPasswordCols+` FROM app_passwords WHERE user_id = $1 AND uuid = $2::uuid`, userID, uuid))
	if err != nil {
		return nil, store.ErrNotFound
	}
	return ap, nil
}

func (s *Server) appPasswordByID(c *gin.Context, id int64) (*appPassword, error) {
	ap, err := scanAppPassword(s.store.Pool.QueryRow(c, `SELECT `+appPasswordCols+` FROM app_passwords WHERE id = $1`, id))
	if err != nil {
		return nil, store.ErrNotFound
	}
	return ap, nil
}

var (
	errAppPasswordEmptyName     = errors.New("An application name is required to create an application password.")
	errAppPasswordDuplicateName = errors.New("Each application name should be unique.")
)

// createAppPassword is WP_Application_Passwords::create_new_application_password(): returns the plaintext
// password (shown once) and the stored record.
func (s *Server) createAppPassword(c *gin.Context, userID int64, name, appID string) (string, *appPassword, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, errAppPasswordEmptyName
	}
	if appID != "" && !isUUID(appID) {
		return "", nil, errors.New("The application ID must be a UUID.")
	}
	existing, err := s.appPasswords(c, userID)
	if err != nil {
		return "", nil, err
	}
	for _, ap := range existing {
		if strings.EqualFold(ap.Name, name) {
			return "", nil, errAppPasswordDuplicateName
		}
	}
	plain := generateAppPassword()
	hash, err := auth.HashPassword(plain)
	if err != nil {
		return "", nil, err
	}
	var id int64
	if err := s.store.Pool.QueryRow(c, `INSERT INTO app_passwords(user_id, uuid, app_id, name, password_hash) VALUES ($1, $2::uuid, $3, $4, $5) RETURNING id`,
		userID, newUUID(), strings.ToLower(appID), name, hash).Scan(&id); err != nil {
		return "", nil, err
	}
	ap, err := s.appPasswordByID(c, id)
	return plain, ap, err
}

func (s *Server) renameAppPassword(c *gin.Context, ap *appPassword, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errAppPasswordEmptyName
	}
	_, err := s.store.Pool.Exec(c, `UPDATE app_passwords SET name = $2 WHERE id = $1`, ap.ID, name)
	ap.Name = name
	return err
}

func (s *Server) deleteAppPassword(c *gin.Context, id int64) error {
	_, err := s.store.Pool.Exec(c, `DELETE FROM app_passwords WHERE id = $1`, id)
	return err
}

func (s *Server) deleteAllAppPasswords(c *gin.Context, userID int64) (int64, error) {
	tag, err := s.store.Pool.Exec(c, `DELETE FROM app_passwords WHERE user_id = $1`, userID)
	return tag.RowsAffected(), err
}

// appPasswordRows fills the profile table (WP_Application_Passwords_List_Table).
func (s *Server) appPasswordRows(c *gin.Context, u *store.User) []appPasswordRow {
	list, _ := s.appPasswords(c, u.ID)
	loc := s.loc()
	var out []appPasswordRow
	for _, ap := range list {
		row := appPasswordRow{appPassword: *ap, Created: ap.CreatedAt.In(loc).Format("January 2, 2006"), Used: "—"}
		if ap.LastUsed != nil {
			row.Used = ap.LastUsed.In(loc).Format("January 2, 2006")
		}
		if row.LastIP == "" {
			row.LastIP = "—"
		}
		out = append(out, row)
	}
	return out
}

// appPasswordsForm handles the profile form's application-password buttons: add, revoke one, revoke all.
func (s *Server) appPasswordsForm(c *gin.Context, target *store.User, back string) {
	u := currentUser(c)
	self := target.ID == u.ID
	render := func(d profileData, notices ...notice) {
		screen, title, body := "profile", "Profile", "profile-php"
		if !self {
			screen, title, body = "users", "Edit User "+target.Login, "user-edit-php"
		}
		s.adminPage(c, screen, title, body, "admin/profile", d, notices...)
	}
	switch {
	case c.PostForm("revoke_all_application_passwords") != "":
		_, _ = s.deleteAllAppPasswords(c, target.ID)
		c.Redirect(http.StatusFound, back)
	case c.PostForm("revoke_application_password") != "":
		if ap, err := s.appPasswordByUUID(c, target.ID, c.PostForm("revoke_application_password")); err == nil {
			_ = s.deleteAppPassword(c, ap.ID)
		}
		c.Redirect(http.StatusFound, back)
	default:
		plain, ap, err := s.createAppPassword(c, target.ID, c.PostForm("new_application_password_name"), "")
		if err != nil {
			render(s.profileView(c, target), notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(err.Error()))})
			return
		}
		d := s.profileView(c, target)
		d.NewAppPassword = &appPasswordCreated{Name: ap.Name, Password: chunkAppPassword(plain)}
		render(d)
	}
}

// authorizeApplication is wp-admin/authorize-application.php.
func (s *Server) authorizeApplication(c *gin.Context) {
	u := currentUser(c)
	d := map[string]any{"AppName": c.Request.FormValue("app_name"), "AppID": c.Request.FormValue("app_id"),
		"SuccessURL": c.Request.FormValue("success_url"), "RejectURL": c.Request.FormValue("reject_url"), "Nonce": s.nonce(c, "authorize_application_password")}
	var errs []string
	for _, k := range []string{"SuccessURL", "RejectURL"} {
		if err := validAuthorizeRedirect(d[k].(string)); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if id := d["AppID"].(string); id != "" && !isUUID(id) {
		errs = append(errs, "The application ID must be a UUID.")
	}
	if len(errs) > 0 {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusBadRequest, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Invalid request</title>`+
			`<link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head><body id="error-page"><div class="wp-die-message"><p>`+template.HTMLEscapeString(strings.Join(errs, " "))+`</p></div></body></html>`)
		return
	}
	if c.Request.Method == http.MethodPost && c.PostForm("action") == "authorize_application_password" {
		if !s.checkNonce(c, "authorize_application_password") {
			s.denyNonce(c)
			return
		}
		if _, reject := c.GetPostForm("reject"); reject {
			target := d["RejectURL"].(string)
			if target == "" {
				target = "/wp-admin/"
			}
			c.Redirect(http.StatusFound, target) // wp_redirect(): the application's own URL
			return
		}
		if _, approve := c.GetPostForm("approve"); approve {
			plain, ap, err := s.createAppPassword(c, u.ID, d["AppName"].(string), d["AppID"].(string))
			if err != nil {
				d["Error"] = err.Error()
			} else if success := d["SuccessURL"].(string); success != "" {
				sep := "?"
				if strings.Contains(success, "?") {
					sep = "&"
				}
				c.Redirect(http.StatusFound, success+sep+"site_url="+url.QueryEscape(s.links(c).HomeURL(""))+"&user_login="+url.QueryEscape(u.Login)+"&password="+url.QueryEscape(plain))
				return
			} else {
				d["NewPassword"] = chunkAppPassword(plain)
				d["AppName"] = ap.Name
			}
		}
	}
	s.adminPage(c, "authorize-application", "Authorize Application", "authorize-application-php", "admin/authorize-application", d)
}

// validAuthorizeRedirect is wp_is_authorize_application_redirect_url_valid(): any scheme but javascript:
// and data:, and http only for local hosts.
func validAuthorizeRedirect(raw string) error {
	if raw == "" {
		return nil
	}
	if !regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`).MatchString(raw) {
		return errors.New("The URL must be served over a secure connection.")
	}
	pu, err := url.Parse(raw)
	if err != nil {
		return errors.New("The URL must be served over a secure connection.")
	}
	switch strings.ToLower(pu.Scheme) {
	case "javascript", "data":
		return errors.New("The URL must be served over a secure connection.")
	case "http":
		if !isLocalHost(pu.Hostname()) {
			return errors.New("The URL must be served over a secure connection.")
		}
	}
	return nil
}
