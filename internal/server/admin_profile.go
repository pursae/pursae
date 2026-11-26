package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/mail"
	"wpgo/internal/store"
)

// profile.php and user-edit.php share one screen: Target is the user being edited.
type profileData struct {
	Target         *store.User
	Self           bool
	Action         string
	Nonce          string
	DisplayOptions []string
	Avatar96       string
	ShowToolbar    bool
	// UpdateEmails: the "Pursae updates" row is shown (eligible user) and its switch is on.
	UpdateEmailsRow, UpdateEmails bool
	// Language: the user's "locale" meta ("" = Site Default) and the installed languages.
	Locale    string
	Languages []langChoice
	// Pursae Classic Editor: the "Default Editor" row, shown while users may switch editors.
	ShowEditorChoice bool
	DefaultEditor    string // "classic" or "block"
	SyntaxOff        bool   // "Disable syntax highlighting when editing code"
	Colors           []adminColor
	Color            string // admin_color
	Themes           []adminTheme
	Theme            string // admin_theme
	ThemeModes       []struct{ Slug, Name string }
	ThemeMode        string // admin_theme_mode (Pursae Modern)
	OtherSessions    int
	AvatarID         int64
	CanUpload        bool
	// Role editing (user-edit.php, for users who can promote_users)
	CanPromote bool
	// CanEditCaps is the freeform capability box. Only an administrator may grant
	// manage_options, edit_users and the other privileged capabilities.
	CanEditCaps bool
	Roles       []roleChoice
	CapRows     []capRow
	// PendingEmail is the _new_email user meta: an e-mail change waiting for confirmation (own profile only).
	PendingEmail string
	DismissURL   string
	// Application Passwords (profile.php's application-passwords section).
	AppPasswords   []appPasswordRow
	OAuthGrants    []oauthGrantRow // connected MCP clients (oauth.go)
	AppScopes      []scopeChoice   // the scope checkboxes of "Add New Application Password"
	NewAppPassword *appPasswordCreated
	// AppPasswordsOn: the user may have Application Passwords (not a front-end member, admin_access.go).
	AppPasswordsOn bool
	// CanAIAccess: the own profile of a user who may open Tools › AI Access (the one-step MCP connect).
	CanAIAccess bool
	// AdminSec is AdminSecGo's block: two-step login status and the session list.
	AdminSec template.HTML
}

type roleChoice struct {
	Slug, Name     string
	Primary, Extra bool
}

type capRow struct {
	Cap, Effect string
}

func (s *Server) profileView(c *gin.Context, target *store.User) profileData {
	u := currentUser(c)
	d := profileData{Target: target, Self: target.ID == u.ID, Nonce: s.nonce(c, "update-user_"+itoa64(target.ID)),
		Avatar96: s.avatarURL(c, target.Email, 96), ShowToolbar: true, Action: "/wp-admin/profile"}
	if !d.Self {
		d.Action = "/wp-admin/user-edit?user_id=" + itoa64(target.ID)
	}
	if v, ok := target.Meta["show_admin_bar_front"].(bool); ok {
		d.ShowToolbar = v
	}
	d.UpdateEmailsRow = updateEmailsEligible(target)
	d.UpdateEmails = updateEmailsOn(target) // off unless the user opted in (hub_contacts.go)
	d.Locale, _ = target.Meta["locale"].(string)
	d.Languages, _ = s.languageChoices(d.Locale, true)
	d.ShowEditorChoice, d.DefaultEditor = s.classicSwitchAllowed(), s.userDefaultEditor(target)
	d.SyntaxOff, d.Colors, d.Color = !syntaxHighlighting(target), adminColors, adminColorFor(target).Slug
	d.Themes, d.Theme = adminThemes, adminThemeFor(target).Slug
	d.ThemeModes, d.ThemeMode = adminThemeModes, adminThemeFor(target).Mode
	if d.ThemeMode == "" {
		d.ThemeMode = "light"
	}
	if d.Theme == "modern" {
		d.Color = adminColors[0].Slug
	}
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			d.DisplayOptions = append(d.DisplayOptions, v)
		}
	}
	first, last := target.Profile["first_name"], target.Profile["last_name"]
	add(target.Profile["nickname"])
	add(target.Login)
	add(first)
	add(last)
	if first != "" && last != "" {
		add(first + " " + last)
		add(last + " " + first)
	}
	add(target.DisplayName)
	if d.Self {
		sess := currentSession(c)
		_ = s.store.Pool.QueryRow(c, `SELECT count(*) FROM sessions WHERE user_id = $1 AND id <> $2 AND expires_at > now()`, target.ID, sess.ID).Scan(&d.OtherSessions)
	}
	if !d.Self && u.Can("promote_users", nil) && s.actorMayEditUserRoles(c, u, target) {
		d.CanPromote = true
		roles, _ := s.store.Roles(c)
		for _, r := range roles {
			if !s.actorMayGiveRole(c, u, r.Slug) {
				continue
			}
			rc := roleChoice{Slug: r.Slug, Name: r.Name}
			for i, have := range target.Roles {
				if have == r.Slug {
					rc.Primary, rc.Extra = i == 0, i > 0
				}
			}
			d.Roles = append(d.Roles, rc)
		}
	}
	if !d.Self && u.Can("manage_options", nil) {
		d.CanEditCaps = true
		exc, _ := s.store.UserCapExceptions(c, target.ID)
		for k, v := range exc {
			d.CapRows = append(d.CapRows, capRow{k, v})
		}
		sort.Slice(d.CapRows, func(i, j int) bool { return d.CapRows[i].Cap < d.CapRows[j].Cap })
	}
	s.avatarPicker(c, target, &d)
	if d.Self {
		if ne := pendingEmail(target); ne != "" && ne != target.Email {
			d.PendingEmail = ne
			d.DismissURL = "/wp-admin/profile?dismiss=" + itoa64(target.ID) + "_new_email&_wpnonce=" + s.nonce(c, "dismiss-"+itoa64(target.ID)+"_new_email")
		}
	}
	d.AppPasswords = s.appPasswordRows(c, target)
	d.AppPasswordsOn = appPasswordsAvailable(target)
	d.AppScopes = scopeChoices(oauthDefaultScopes)
	d.CanAIAccess = d.Self && currentUser(c).Can("manage_options", nil)
	d.OAuthGrants = s.oauthGrantRows(c, target.ID)
	d.AdminSec = s.adminsecProfileHTML(c, target, d.Self)
	return d
}

// pendingEmail reads the _new_email user meta (send_confirmation_on_profile_email()).
func pendingEmail(u *store.User) string {
	rec, _ := u.Meta["_new_email"].(map[string]any)
	ne, _ := rec["newemail"].(string)
	return ne
}

// profile is profile.php: the screen, plus the ?newuseremail= confirmation and ?dismiss= cancel links
// of a pending e-mail change.
func (s *Server) profile(c *gin.Context) {
	u := currentUser(c)
	if hash := c.Query("newuseremail"); hash != "" {
		ok := s.confirmEmailChange(c, u, hash)
		if frontEndOnly(u) {
			// Customers and forum members confirm here too (denyFrontEndAdmin lets this link through),
			// then go back to the front end.
			s.frontEndEmailConfirmed(c, ok)
			return
		}
		if ok {
			c.Redirect(http.StatusFound, "/wp-admin/profile?updated=1")
		} else {
			c.Redirect(http.StatusFound, "/wp-admin/profile?error=new-email")
		}
		return
	}
	if d := c.Query("dismiss"); d != "" && d == itoa64(u.ID)+"_new_email" {
		if !s.checkNonce(c, "dismiss-"+itoa64(u.ID)+"_new_email") {
			s.denyNonce(c)
			return
		}
		_ = s.setUserMeta(c, u, "_new_email", nil)
		c.Redirect(http.StatusFound, "/wp-admin/profile?updated=1")
		return
	}
	var notices []notice
	if c.Query("error") == "new-email" {
		notices = append(notices, notice{Type: "error", Message: "Error while saving the new email address. Please try again."})
	}
	if c.Query("updated") == "1" {
		if ne := pendingEmail(u); ne != "" {
			notices = append(notices, notice{Type: "warning", Message: template.HTML("Your email address has not been updated yet. Please check your inbox at <code>" + template.HTMLEscapeString(ne) + "</code> for a confirmation email."), Dismissible: true})
		}
	}
	s.adminPage(c, "profile", "Profile", "profile-php", "admin/profile", s.profileView(c, u), notices...)
}

func (s *Server) profileSave(c *gin.Context) {
	s.saveUser(c, currentUser(c))
}

// userEdit is user-edit.php.
func (s *Server) userEdit(c *gin.Context) {
	u := currentUser(c)
	id, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	if id == u.ID {
		c.Redirect(http.StatusFound, "/wp-admin/profile")
		return
	}
	target, err := s.store.UserByID(c, id)
	if err != nil {
		c.String(http.StatusNotFound, "Invalid user ID.")
		return
	}
	if !u.Can("edit_users", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit this user.")
		return
	}
	if c.Request.Method == http.MethodPost {
		s.saveUser(c, target)
		return
	}
	var notices []notice
	if c.Query("updated") == "1" {
		notices = append(notices, notice{Type: "success", Message: "<strong>User updated.</strong> <a href=\"/wp-admin/users\">&larr; Go to Users</a>", Dismissible: true})
	}
	s.adminPage(c, "users", "Edit User "+target.Login, "user-edit-php", "admin/profile", s.profileView(c, target), notices...)
}

// saveUser is edit_user(): profile fields, password, avatar and, for other users, roles and capabilities.
func (s *Server) saveUser(c *gin.Context, target *store.User) {
	u := currentUser(c)
	self := target.ID == u.ID
	if !s.checkNonce(c, "update-user_"+itoa64(target.ID)) {
		s.denyNonce(c)
		return
	}
	back := "/wp-admin/profile?updated=1"
	if !self {
		back = "/wp-admin/user-edit?user_id=" + itoa64(target.ID) + "&updated=1"
	}
	if s.adminsecProfileAction(c, target, back) { // AdminSecGo: session and factor buttons
		return
	}
	if self && c.PostForm("action") == "destroy-sessions" {
		if err := s.auth.DestroyOthers(c, u.ID, currentSession(c).ID); err != nil {
			// The other sessions are still live: say so in the log rather than drop the error.
			slog.Error("profile: log out everywhere else failed", "user_id", u.ID, "err", err)
		}
		c.Redirect(http.StatusFound, back)
		return
	}
	if id := c.PostForm("revoke_oauth_grant"); id != "" {
		_, _ = s.store.Pool.Exec(c, `DELETE FROM oauth_tokens WHERE id::text = $1 AND user_id = $2`, id, target.ID)
		c.Redirect(http.StatusFound, back)
		return
	}
	if _, ok := c.GetPostForm("do_new_application_password"); ok || c.PostForm("revoke_application_password") != "" || c.PostForm("revoke_all_application_passwords") != "" {
		s.appPasswordsForm(c, target, back)
		return
	}
	var errs []string
	nickname := strings.TrimSpace(c.PostForm("nickname"))
	email := strings.TrimSpace(c.PostForm("email"))
	if nickname == "" {
		errs = append(errs, "<strong>Error:</strong> Please enter a nickname.")
	}
	if !validEmail(email) {
		errs = append(errs, "<strong>Error:</strong> Please enter an email address.")
	} else if !strings.EqualFold(email, target.Email) {
		if _, taken, _ := s.store.LoginOrEmailTaken(c, "", email); taken {
			errs = append(errs, "<strong>Error:</strong> The email address is already used.")
		}
	}
	pass1, pass2 := c.PostForm("pass1"), c.PostForm("pass2")
	if pass1 != "" && pass2 != "" && pass1 != pass2 {
		errs = append(errs, "<strong>Error:</strong> Passwords do not match. Please enter the same password in both password fields.")
	} else if pass1 != "" {
		if msg := s.adminsecUserPasswordProblem(target, pass1); msg != "" { // AdminSecGo password policy
			errs = append(errs, "<strong>Error:</strong> "+template.HTMLEscapeString(msg))
		}
	}
	if !self && u.Can("promote_users", nil) {
		if primary, ok := c.GetPostForm("role"); ok {
			if msg := s.roleAssignmentProblem(c, u, target, primary, c.PostFormArray("extra_roles[]")); msg != "" {
				errs = append(errs, "<strong>Error:</strong> "+template.HTMLEscapeString(msg))
			}
		}
	}
	if !self && !u.Can("manage_options", nil) {
		if msg := capAssignmentProblem(u, c.PostFormArray("cap_name[]"), c.PostFormArray("cap_effect[]")); msg != "" {
			errs = append(errs, "<strong>Error:</strong> "+template.HTMLEscapeString(msg))
		}
	}
	if len(errs) > 0 {
		var ns []notice
		for _, e := range errs {
			ns = append(ns, notice{Type: "error", Message: template.HTML(e)})
		}
		screen, title, body := "profile", "Profile", "profile-php"
		if !self {
			screen, title, body = "users", "Edit User "+target.Login, "user-edit-php"
		}
		s.adminPage(c, screen, title, body, "admin/profile", s.profileView(c, target), ns...)
		return
	}
	target.Profile["first_name"] = strings.TrimSpace(c.PostForm("first_name"))
	target.Profile["last_name"] = strings.TrimSpace(c.PostForm("last_name"))
	target.Profile["nickname"] = nickname
	target.Profile["description"] = strings.TrimSpace(c.PostForm("description"))
	if dn := strings.TrimSpace(c.PostForm("display_name")); dn != "" {
		target.DisplayName = dn
	}
	if self && !strings.EqualFold(email, target.Email) {
		// send_confirmation_on_profile_email(): the new address becomes active once confirmed.
		s.sendEmailChangeConfirmation(c, target, email)
	} else {
		target.Email = email
	}
	target.URL = userURL(c.PostForm("url")) // pre_user_url: esc_url_raw
	if err := s.store.UpdateUserProfile(c, target); err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.setUserMeta(c, target, "show_admin_bar_front", c.PostForm("admin_bar_front") == "1")
	if _, shown := c.GetPostForm("pursae_update_emails_shown"); shown && updateEmailsEligible(target) {
		// Consent is the user's own: only they can turn update e-mails on (recording the time); anyone
		// who may edit the profile can turn them off (hub_contacts.go).
		want, on := c.PostForm("pursae_update_emails") == "1", updateEmailsOn(target)
		switch {
		case want && !on && self:
			_ = s.setUpdateEmails(c, target, true)
		case !want && (self || on): // another user who never opted in keeps being asked by the notice
			_ = s.setUpdateEmails(c, target, false)
		}
	}
	if loc, ok := c.GetPostForm("locale"); ok {
		if loc == "site-default" {
			loc = ""
		}
		if loc == "" || loc == "en_US" || validLanguage(loc) && s.lang.IsInstalled(loc) {
			_ = s.setUserMeta(c, target, "locale", loc)
		}
	}
	if v := c.PostForm("classic-editor-user-settings"); s.classicSwitchAllowed() && (v == "classic" || v == "block") {
		_ = s.setUserMeta(c, target, "classic-editor-settings", v)
	}
	if c.PostForm("syntax_highlighting") == "false" {
		_ = s.setUserMeta(c, target, "syntax_highlighting", "false")
	} else {
		_ = s.setUserMeta(c, target, "syntax_highlighting", "true")
	}
	for _, sc := range adminColors {
		if sc.Slug == c.PostForm("admin_color") {
			_ = s.setUserMeta(c, target, "admin_color", sc.Slug)
		}
	}
	for _, t := range adminThemes {
		if t.Slug == c.PostForm("admin_theme") {
			_ = s.setUserMeta(c, target, "admin_theme", t.Slug)
		}
	}
	if m := c.PostForm("admin_theme_mode"); validThemeMode(m) {
		_ = s.setUserMeta(c, target, "admin_theme_mode", m)
	}
	s.saveAvatar(c, target)
	if pass1 != "" {
		if hash, err := auth.HashPassword(pass1); err == nil {
			if s.store.SetUserPassword(c, target.ID, hash) == nil {
				s.endSessionsAfterPasswordChange(c, c, target.ID)
				s.revokeCredentialsAfterPasswordChange(c, target.ID)
				s.adminsecPasswordChanged(c, target)
			}
		}
	}
	if !self && u.Can("promote_users", nil) {
		if primary, ok := c.GetPostForm("role"); ok {
			before := strings.Join(target.Roles, ",")
			extras := c.PostFormArray("extra_roles[]")
			_ = s.store.SetUserRoles(c, target.ID, primary, extras)
			s.adminsecRoleChanged(c, target.ID, target.Login, before, strings.Join(append([]string{primary}, extras...), ","))
		}
	}
	// Freeform capability grants (manage_options, edit_users, …) are an administrator action.
	if !self && u.Can("manage_options", nil) {
		exc := map[string]string{}
		caps := c.PostFormArray("cap_name[]")
		effects := c.PostFormArray("cap_effect[]")
		for i, cp := range caps {
			cp = strings.TrimSpace(cp)
			if cp != "" && i < len(effects) && (effects[i] == "allow" || effects[i] == "deny") {
				exc[cp] = effects[i]
			}
		}
		_ = s.store.SetUserCapExceptions(c, target.ID, exc)
	}
	s.purgeAll(c) // names and avatars appear in bylines and comment lists across the site
	c.Redirect(http.StatusFound, back)
}

// confirmEmailChange applies the pending _new_email address when hash matches its link.
func (s *Server) confirmEmailChange(c *gin.Context, u *store.User, hash string) bool {
	rec, _ := u.Meta["_new_email"].(map[string]any)
	want, _ := rec["hash"].(string)
	ne, _ := rec["newemail"].(string)
	if want == "" || ne == "" || subtle.ConstantTimeCompare([]byte(want), []byte(hash)) != 1 {
		return false
	}
	// The address was free when the change was requested; someone may have registered it since.
	if _, taken, err := s.store.LoginOrEmailTaken(c, "", ne); err != nil || (taken && !strings.EqualFold(ne, u.Email)) {
		return false
	}
	u.Email = ne
	if err := s.store.UpdateUserProfile(c, u); err != nil {
		return false
	}
	_ = s.setUserMeta(c, u, "_new_email", nil)
	s.purgeAll(c)
	return true
}

// frontEndEmailConfirmed sends a front-end member back from the confirmation link: to My Account's
// account details with a notice while the store is on, otherwise to their front-end home.
func (s *Server) frontEndEmailConfirmed(c *gin.Context, ok bool) {
	if s.gooOn() {
		if ok {
			s.gooAddNotice(c, "success", "Your email address has been updated.")
		} else {
			s.gooAddNotice(c, "error", "Error while saving the new email address. Please try again.")
		}
		s.gooRedirect(c, s.gooAccountURL(c.Request.Context(), "edit-account"))
		return
	}
	c.Redirect(http.StatusFound, s.frontEndHome(c))
}

// sendEmailChangeConfirmation is send_confirmation_on_profile_email(): stores the pending address in
// the _new_email user meta and mails the confirmation link to it.
func (s *Server) sendEmailChangeConfirmation(c *gin.Context, u *store.User, newEmail string) {
	sum := sha256.Sum256([]byte(newEmail + itoa64(time.Now().UnixNano()) + auth.RandomToken(8)))
	hash := hex.EncodeToString(sum[:16])
	_ = s.setUserMeta(c, u, "_new_email", map[string]any{"hash": hash, "newemail": newEmail})
	site := s.settings.String("blogname")
	body := "Howdy ###USERNAME###,\n\nYou recently requested to have the email address on your account changed.\n\n" +
		"If this is correct, please click on the following link to change it:\n###ADMIN_URL###\n\n" +
		"You can safely ignore and delete this email if you do not want to\ntake this action.\n\n" +
		"This email has been sent to ###EMAIL###\n\nRegards,\nAll at ###SITENAME###\n###SITEURL###\n"
	body = strings.NewReplacer("###USERNAME###", u.Login, "###ADMIN_URL###", s.configuredLinks(c).HomeURL("/wp-admin/profile?newuseremail="+hash),
		"###EMAIL###", newEmail, "###SITENAME###", site, "###SITEURL###", s.links(c).HomeURL("/")).Replace(body)
	s.sendMail(c, mail.Message{Kind: "user_email_change", To: []string{newEmail}, Subject: "[" + site + "] Email Change Request", Body: body})
}

// avatarPicker wires the profile picture field to the media modal (wp.media), the same frame used for
// featured images: pick from the library or upload. WordPress Go never calls Gravatar.
func (s *Server) avatarPicker(c *gin.Context, u *store.User, d *profileData) {
	// avatarIDOf accepts a number (admin saves) or a numeric string (CLI/imports); reading only float64
	// would render 0 for a string and the next profile save would drop the picture.
	d.AvatarID = avatarIDOf(u)
	d.CanUpload = currentUser(c).Can("upload_files", nil)
	if !d.CanUpload {
		return
	}
	s.enqueueMedia(c, 0)
	s.pageAssets(c).AddInline("media-editor", `jQuery( function( $ ) {
	var frame, $img = $( '.user-profile-picture img.avatar' ), fallback = `+strconvQuote(defaultAvatar)+`;
	$( '#wpgo-avatar-choose' ).on( 'click', function( event ) {
		event.preventDefault();
		if ( ! frame ) {
			frame = wp.media( {
				title: 'Choose Profile Picture',
				library: { type: 'image' },
				button: { text: 'Use as profile picture' },
				multiple: false
			} );
			frame.on( 'open', function() {
				var id = parseInt( $( '#avatar_id' ).val(), 10 ), selection = frame.state().get( 'selection' );
				if ( id > 0 ) {
					var attachment = wp.media.attachment( id );
					attachment.fetch();
					selection.reset( [ attachment ] );
				}
			} );
			frame.on( 'select', function() {
				var a = frame.state().get( 'selection' ).first().toJSON(),
					size = a.sizes && ( a.sizes.thumbnail || a.sizes.medium || a.sizes.full ),
					src = size ? size.url : a.url;
				$( '#avatar_id' ).val( a.id );
				$img.attr( { src: src, srcset: src + ' 2x' } );
				$( '#wpgo-avatar-choose' ).text( 'Change Profile Picture' );
				$( '#wpgo-avatar-remove' ).prop( 'hidden', false );
				wp.a11y.speak( 'Profile picture selected. Save your profile to keep it.' );
			} );
		}
		frame.open();
	} );
	$( '#wpgo-avatar-remove' ).on( 'click', function( event ) {
		event.preventDefault();
		$( '#avatar_id' ).val( '0' );
		$img.attr( { src: fallback, srcset: fallback + ' 2x' } );
		$( '#wpgo-avatar-choose' ).text( 'Choose Profile Picture' );
		$( this ).prop( 'hidden', true );
	} );
} );`)
}

// saveAvatar stores the chosen attachment as the user's profile picture (users.meta.avatar_id).
func (s *Server) saveAvatar(c *gin.Context, u *store.User) {
	raw, ok := c.GetPostForm("avatar_id")
	if !ok || !currentUser(c).Can("upload_files", nil) {
		return
	}
	id, _ := strconv.ParseInt(raw, 10, 64)
	if id > 0 {
		a, err := s.store.GetAttachment(c, id)
		if err != nil || !strings.HasPrefix(a.Mime, "image/") {
			return
		}
	} else {
		id = 0
	}
	_ = s.setUserMeta(c, u, "avatar_id", id)
}
