package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/store"
)

type userRow struct {
	ID                 int64
	Login, Name, Email string
	Avatar             template.HTML
	RoleNames          string
	Posts              int
	EditURL, DeleteURL string
	ResetURL           string // "Send password reset" row action (users.php?action=resetpassword)
	PostsURL           string
	IsSelf             bool
	Security           template.HTML // AdminSecGo's Two-Step column
}

type usersListData struct {
	Rows       []userRow
	Role       string
	Search     string
	Views      []roleView
	Pagination pagination
	BulkNonce  string
	Roles      []store.Role
	SortLinks  map[string]sortLink
	SecColumn  bool // AdminSecGo is active: the Two-Step column
}

type roleView struct {
	Slug, Name string
	Count      int
	Current    bool
}

// usersScreen is users.php.
func (s *Server) usersScreen(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("list_users", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to list users.")
		return
	}
	ctx := c.Request.Context()
	formValue := func(key string) string {
		if c.Request.Method == http.MethodPost {
			if v := c.PostForm(key); v != "" {
				return v
			}
		}
		return c.Query(key)
	}
	action := formValue("action")
	if action == "" || action == "-1" {
		action = formValue("action2")
	}
	switch action {
	case "delete":
		s.userDeleteConfirm(c)
		return
	case "dodelete":
		s.userDelete(c)
		return
	case "resetpassword":
		s.usersSendReset(c)
		return
	}
	// Bulk role change is POST-only. A GET link must not promote users.
	if c.Request.Method == http.MethodPost {
		r := c.PostForm("new_role")
		if c.PostForm("changeit2") != "" { // bottom "Change role to…" control
			r = c.PostForm("new_role2")
		}
		if r != "" && (c.PostForm("changeit") != "" || c.PostForm("changeit2") != "") {
			if !s.checkNonce(c, "bulk-users") || !u.Can("promote_users", nil) {
				s.denyNonce(c)
				return
			}
			known, _ := s.store.Roles(ctx)
			if !roleListed(known, r) || !s.actorMayGiveRole(ctx, u, r) {
				c.String(http.StatusForbidden, "Sorry, you are not allowed to give users that role.")
				return
			}
			for _, raw := range selectedUserIDs(c) {
				if raw == u.ID {
					continue
				}
				x, err := s.store.UserByID(ctx, raw)
				if err != nil || !s.actorMayEditUserRoles(ctx, u, x) || s.removingLastAdmin(ctx, x, []string{r}) {
					continue
				}
				before, access := strings.Join(x.Roles, ","), s.userAccessKey(ctx, raw)
				_ = s.store.SetUserRoles(ctx, raw, r, nil)
				s.endSessionsAfterAccessChange(ctx, c, raw, access)
				s.adminsecRoleChanged(c, raw, x.Login, before, r)
			}
			c.Redirect(http.StatusFound, "/wp-admin/users?update=promote")
			return
		}
		// Search is not a mutation. Send it back to a GET so pagination and role
		// views keep the filter in the query string.
		qv := url.Values{}
		if search := strings.TrimSpace(c.PostForm("s")); search != "" {
			qv.Set("s", search)
		}
		if role := c.PostForm("role"); role != "" {
			qv.Set("role", role)
		}
		c.Redirect(http.StatusFound, "/wp-admin/users?"+qv.Encode())
		return
	}
	page, _ := strconv.Atoi(c.Query("paged"))
	q := store.UserQuery{Role: c.Query("role"), Search: strings.TrimSpace(c.Query("s")), Page: page, PerPage: 20,
		OrderBy: c.Query("orderby"), Order: c.Query("order")}
	list, total, err := s.store.QueryUsers(ctx, q)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	roles, _ := s.store.Roles(ctx)
	names := map[string]string{}
	for _, r := range roles {
		names[r.Slug] = r.Name
	}
	counts, _ := s.store.RoleCounts(ctx)
	var assignable []store.Role
	for _, role := range roles {
		if s.actorMayGiveRole(ctx, u, role.Slug) {
			assignable = append(assignable, role)
		}
	}
	d := usersListData{Role: q.Role, Search: q.Search, BulkNonce: s.nonce(c, "bulk-users"), Roles: assignable,
		SortLinks: sortLinks(c, map[string]bool{"login": false, "email": false}, "login", "asc")}
	d.Views = append(d.Views, roleView{Slug: "", Name: "All", Count: counts["all"], Current: q.Role == ""})
	for _, r := range roles {
		if counts[r.Slug] > 0 {
			d.Views = append(d.Views, roleView{Slug: r.Slug, Name: r.Name, Count: counts[r.Slug], Current: q.Role == r.Slug})
		}
	}
	var ids []int64
	var emails []string
	for _, x := range list {
		ids = append(ids, x.ID)
		emails = append(emails, x.Email)
	}
	postCounts, _ := s.store.UserPostCounts(ctx, ids)
	ctx = withViewMemo(ctx)
	s.prefetchAvatars(ctx, emails) // the rows' avatars in one query each for users and images
	for _, x := range list {
		var rn []string
		for _, r := range x.Roles {
			rn = append(rn, names[r])
		}
		row := userRow{ID: x.ID, Login: x.Login, Email: x.Email, Avatar: template.HTML(s.avatarImg(ctx, x.Email, 32)),
			Name: strings.TrimSpace(x.Profile["first_name"] + " " + x.Profile["last_name"]), RoleNames: strings.Join(rn, ", "),
			Posts: postCounts[x.ID], IsSelf: x.ID == u.ID, EditURL: "/wp-admin/user-edit?user_id=" + itoa64(x.ID),
			PostsURL: "/wp-admin/edit?author=" + itoa64(x.ID)}
		if row.IsSelf {
			row.EditURL = "/wp-admin/profile"
		} else {
			if u.Can("delete_users", nil) {
				row.DeleteURL = "/wp-admin/users?action=delete&user=" + itoa64(x.ID) + "&_wpnonce=" + s.nonce(c, "bulk-users")
			}
			if u.Can("edit_users", nil) {
				row.ResetURL = "/wp-admin/users?action=resetpassword&users=" + itoa64(x.ID) + "&_wpnonce=" + s.nonce(c, "bulk-users")
			}
		}
		d.Rows = append(d.Rows, row)
	}
	d.SecColumn = s.adminsecUsersColumn(c, d.Rows, list) // AdminSecGo: two-step login status
	d.Pagination = newPagination(total, 20, max(page, 1), *c.Request.URL)
	var notices []notice
	switch c.Query("update") {
	case "add":
		notices = append(notices, notice{Type: "success", Message: "New user created.", Dismissible: true})
	case "del":
		notices = append(notices, notice{Type: "success", Message: "User deleted.", Dismissible: true})
	case "promote":
		notices = append(notices, notice{Type: "success", Message: "Changed roles.", Dismissible: true})
	case "resetpassword":
		if n, _ := strconv.Atoi(c.Query("reset_count")); n == 1 {
			notices = append(notices, notice{Type: "success", Message: "Password reset link sent to 1 user.", Dismissible: true})
		} else if n > 1 {
			notices = append(notices, notice{Type: "success", Message: template.HTML(template.HTMLEscapeString(fmt.Sprintf("Password reset links sent to %d users.", n))), Dismissible: true})
		}
	case "err_admin_reset":
		notices = append(notices, notice{Type: "error", Message: "Sorry, you are not allowed to send a password reset link for that user.", Dismissible: true})
	}
	s.adminPage(c, "users", "Users", "users-php", "admin/users-list", d, notices...)
}

type userDeleteData struct {
	Targets  []*store.User
	Others   []*store.User
	Nonce    string
	HasPosts bool
}

// selectedUserIDs reads the users of a row action (user=, users=) or a bulk action (users[]=), never the current user.
func selectedUserIDs(c *gin.Context) []int64 {
	raw := append(c.QueryArray("users[]"), c.QueryArray("users")...)
	raw = append(raw, c.PostFormArray("users[]")...)
	raw = append(raw, c.PostFormArray("users")...)
	if v := c.Query("user"); v != "" {
		raw = append(raw, v)
	}
	if v := c.PostForm("user"); v != "" {
		raw = append(raw, v)
	}
	var ids []int64
	seen := map[int64]bool{}
	for _, r := range raw {
		if id, err := strconv.ParseInt(r, 10, 64); err == nil && id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Server) userDeleteConfirm(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("delete_users", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to delete users.")
		return
	}
	if !s.checkNonce(c, "bulk-users") {
		s.denyNonce(c)
		return
	}
	d := userDeleteData{Nonce: s.nonce(c, "delete-users")}
	ids := map[int64]bool{}
	for _, id := range selectedUserIDs(c) {
		if id == u.ID {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to delete this user.")
			return
		}
		target, err := s.store.UserByID(c, id)
		if err != nil {
			c.String(http.StatusNotFound, "Invalid user ID.")
			return
		}
		d.Targets = append(d.Targets, target)
		ids[id] = true
		var n int
		_ = s.store.Pool.QueryRow(c, `SELECT count(*) FROM posts WHERE author_id = $1`, id).Scan(&n)
		d.HasPosts = d.HasPosts || n > 0
	}
	if len(d.Targets) == 0 {
		c.Redirect(http.StatusFound, "/wp-admin/users")
		return
	}
	all, _, _ := s.store.QueryUsers(c, store.UserQuery{PerPage: 500})
	for _, x := range all {
		if !ids[x.ID] {
			d.Others = append(d.Others, x)
		}
	}
	s.adminPage(c, "users", "Delete Users", "users-php", "admin/user-delete", d)
}

func (s *Server) userDelete(c *gin.Context) {
	u := currentUser(c)
	if c.Request.Method != http.MethodPost {
		c.String(http.StatusMethodNotAllowed, "User deletion must be confirmed with a POST request.")
		return
	}
	if !u.Can("delete_users", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to delete users.")
		return
	}
	if !s.checkNonce(c, "delete-users") {
		s.denyNonce(c)
		return
	}
	opt := c.PostForm("delete_option")
	if opt == "" {
		opt = c.Query("delete_option")
	}
	reassignRaw := c.PostForm("reassign_user")
	if reassignRaw == "" {
		reassignRaw = c.Query("reassign_user")
	}
	var reassign int64
	if opt == "reassign" {
		reassign, _ = strconv.ParseInt(reassignRaw, 10, 64)
	}
	for _, id := range selectedUserIDs(c) {
		if id == u.ID || id == reassign {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to delete this user.")
			return
		}
		if x, err := s.store.UserByID(c, id); err == nil && s.removingLastAdmin(c, x, nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to delete the last administrator.")
			return
		}
		victim := strconv.FormatInt(id, 10)
		if x, err := s.store.UserByID(c, id); err == nil {
			victim = x.Login
		}
		if err := s.store.DeleteUser(c, id, reassign); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		s.asAudit(c, asEvent{Event: "user_deleted", Severity: "warning", Object: victim, Message: "User " + victim + " deleted."})
	}
	s.purgeAll(c)
	c.Redirect(http.StatusFound, "/wp-admin/users?update=del")
}

// usersSendReset is users.php?action=resetpassword (row action and bulk action): retrieve_password() for each user.
func (s *Server) usersSendReset(c *gin.Context) {
	u := currentUser(c)
	if !s.checkNonce(c, "bulk-users") {
		s.denyNonce(c)
		return
	}
	if !u.Can("edit_users", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit users.")
		return
	}
	ids := selectedUserIDs(c)
	if len(ids) == 0 {
		c.Redirect(http.StatusFound, "/wp-admin/users")
		return
	}
	count, self := 0, false
	for _, id := range ids {
		if id == u.ID {
			self = true // "Sorry, you are not allowed to send a password reset link for that user."
			continue
		}
		if target, err := s.store.UserByID(c, id); err == nil {
			s.sendResetLink(c, target)
			count++
		}
	}
	if self {
		c.Redirect(http.StatusFound, "/wp-admin/users?update=err_admin_reset")
		return
	}
	c.Redirect(http.StatusFound, "/wp-admin/users?update=resetpassword&reset_count="+strconv.Itoa(count))
}

type userNewData struct {
	Nonce  string
	Roles  []store.Role
	Def    string
	Error  template.HTML
	Values url.Values
	// CanPromote shows the Role field (user-new.php: current_user_can( 'promote_users' )).
	CanPromote bool
}

// roleListed reports whether slug is one of the site's roles.
func roleListed(roles []store.Role, slug string) bool {
	for _, r := range roles {
		if r.Slug == slug {
			return true
		}
	}
	return false
}

// privilegedUserCaps cannot be granted through a freeform capability box, or via a
// role, unless the actor is an administrator (manage_options).
var privilegedUserCaps = []string{
	"manage_options", "edit_users", "promote_users", "delete_users", "create_users", "list_users", "remove_users",
	"install_plugins", "activate_plugins", "edit_plugins", "update_plugins", "delete_plugins",
	"edit_files", "update_core", "unfiltered_html",
	"install_themes", "switch_themes", "edit_themes", "delete_themes", "edit_theme_options",
	"export", "import",
}

func privilegedCap(cap string) bool {
	for _, c := range privilegedUserCaps {
		if c == cap {
			return true
		}
	}
	return false
}

func levelOf(caps map[string]bool) int {
	n := 0
	for i := 0; i <= 10; i++ {
		if caps["level_"+strconv.Itoa(i)] {
			n = i
		}
	}
	return n
}

func actorLevel(u *store.User) int {
	if u == nil {
		return -1
	}
	n := 0
	for i := 0; i <= 10; i++ {
		if u.Can("level_"+strconv.Itoa(i), nil) {
			n = i
		}
	}
	return n
}

func (s *Server) roleCapabilityMap(ctx context.Context, slug string) (map[string]bool, bool) {
	var raw []byte
	err := s.store.Pool.QueryRow(ctx, `SELECT capabilities FROM roles WHERE slug = $1`, slug).Scan(&raw)
	if err != nil {
		return nil, false
	}
	m := map[string]bool{}
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

// actorMayGiveRole reports whether actor may put slug on somebody.
// Administrators may assign any real role. Everyone else must already hold the
// role's privileged capabilities, and the role's user level must not exceed theirs.
func (s *Server) actorMayGiveRole(ctx context.Context, actor *store.User, slug string) bool {
	if actor == nil || slug == "" || !actor.Can("promote_users", nil) {
		return false
	}
	caps, ok := s.roleCapabilityMap(ctx, slug)
	if !ok {
		return false
	}
	if actor.Can("manage_options", nil) {
		return true
	}
	if levelOf(caps) > actorLevel(actor) {
		return false
	}
	for _, cap := range privilegedUserCaps {
		if caps[cap] && !actor.Can(cap, nil) {
			return false
		}
	}
	return true
}

// actorMayEditUserRoles reports whether actor may change target's roles.
// A user whose current role is above the actor (an administrator, for a promoter
// who is not one) cannot be edited.
func (s *Server) actorMayEditUserRoles(ctx context.Context, actor, target *store.User) bool {
	if actor == nil || !actor.Can("promote_users", nil) {
		return false
	}
	if target == nil || actor.Can("manage_options", nil) {
		return true
	}
	for _, slug := range target.Roles {
		if !s.actorMayGiveRole(ctx, actor, slug) {
			return false
		}
	}
	return true
}

// removingLastAdmin reports whether newRoles would leave the site with no administrator.
// A nil newRoles means the user is being deleted.
func (s *Server) removingLastAdmin(ctx context.Context, target *store.User, newRoles []string) bool {
	if target == nil {
		return false
	}
	isAdmin := false
	for _, slug := range target.Roles {
		if slug == "administrator" {
			isAdmin = true
		}
	}
	if !isAdmin {
		return false
	}
	for _, slug := range newRoles {
		if slug == "administrator" {
			return false
		}
	}
	var n int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(DISTINCT ur.user_id) FROM user_roles ur
		JOIN roles r ON r.id = ur.role_id WHERE r.slug = 'administrator'`).Scan(&n); err != nil {
		return true // fail closed: do not drop an administrator we cannot count
	}
	return n <= 1
}

// roleAssignmentProblem is the admin-screen check for a posted primary role and extras.
// An empty primary role is "No role for this site".
func (s *Server) roleAssignmentProblem(ctx context.Context, actor, target *store.User, primary string, extra []string) string {
	if !s.actorMayEditUserRoles(ctx, actor, target) {
		return "Sorry, you are not allowed to edit the roles of this user."
	}
	known, _ := s.store.Roles(ctx)
	var next []string
	if primary != "" {
		if !roleListed(known, primary) || !s.actorMayGiveRole(ctx, actor, primary) {
			return "Sorry, you are not allowed to give users that role."
		}
		next = append(next, primary)
	}
	for _, ex := range extra {
		ex = strings.TrimSpace(ex)
		if ex == "" || ex == primary {
			continue
		}
		if !roleListed(known, ex) || !s.actorMayGiveRole(ctx, actor, ex) {
			return "Sorry, you are not allowed to give users that role."
		}
		next = append(next, ex)
	}
	if s.removingLastAdmin(ctx, target, next) {
		return "Sorry, you are not allowed to remove the last administrator."
	}
	return ""
}

// capAssignmentProblem rejects freeform grants of manage_options, edit_users and
// the other privileged capabilities unless the actor is an administrator.
func capAssignmentProblem(actor *store.User, names, effects []string) string {
	if actor != nil && actor.Can("manage_options", nil) {
		return ""
	}
	for i, cp := range names {
		cp = strings.TrimSpace(cp)
		if cp == "" {
			continue
		}
		effect := ""
		if i < len(effects) {
			effect = effects[i]
		}
		if effect == "allow" && (privilegedCap(cp) || actor == nil || !actor.Can(cp, nil)) {
			return "Sorry, you are not allowed to give users that capability."
		}
	}
	return ""
}

// userNew is user-new.php.
func (s *Server) userNew(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("create_users", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to create users.")
		return
	}
	roles, _ := s.store.Roles(c)
	// The role dropdown lists only the roles this user may give (as on the Users screen).
	var assignable []store.Role
	for _, r := range roles {
		if s.actorMayGiveRole(c, u, r.Slug) {
			assignable = append(assignable, r)
		}
	}
	d := userNewData{Nonce: s.nonce(c, "create-user"), Roles: assignable, Def: s.settings.String("default_role"), Values: url.Values{},
		CanPromote: u.Can("promote_users", nil)}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "create-user") {
			s.denyNonce(c)
			return
		}
		_ = c.Request.ParseForm()
		d.Values = c.Request.PostForm
		login := strings.TrimSpace(c.PostForm("user_login"))
		email := strings.TrimSpace(c.PostForm("email"))
		pass := c.PostForm("pass1")
		role := c.PostForm("role")
		// edit_user(): the posted role counts only for users who can promote, and must exist.
		// A promoter may only give a role they are allowed to (actorMayGiveRole), as on the
		// bulk role change, user-edit and REST.
		promoter := u.Can("promote_users", nil)
		if !promoter || role == "" {
			role = d.Def
		}
		var errs []string
		if !roleListed(roles, role) || (promoter && !s.actorMayGiveRole(c, u, role)) {
			errs = append(errs, "<strong>Error:</strong> Sorry, you are not allowed to give users that role.")
		}
		if login == "" {
			errs = append(errs, "<strong>Error:</strong> Please enter a username.")
		} else if !validUsername.MatchString(login) {
			errs = append(errs, "<strong>Error:</strong> This username is invalid because it uses illegal characters. Please enter a valid username.")
		}
		if _, err := mail.ParseAddress(email); err != nil || email == "" || !validEmail(email) {
			errs = append(errs, "<strong>Error:</strong> The email address is not correct.")
		}
		if pass == "" {
			errs = append(errs, "<strong>Error:</strong> Please enter a password.")
		} else if msg := s.adminsecPasswordProblem([]string{role}, login, email, pass); msg != "" { // AdminSecGo password policy
			errs = append(errs, "<strong>Error:</strong> "+template.HTMLEscapeString(msg))
		}
		if lt, et, _ := s.store.LoginOrEmailTaken(c, login, email); lt {
			errs = append(errs, "<strong>Error:</strong> This username is already registered. Please choose another one.")
		} else if et {
			errs = append(errs, "<strong>Error:</strong> This email is already registered. Please choose another one.")
		}
		if len(errs) == 0 {
			hash, err := auth.HashPassword(pass)
			var newID int64
			if err == nil {
				newID, err = s.store.CreateUser(c, store.NewUser{Login: login, Email: email, PasswordHash: hash, Role: role,
					Profile: map[string]string{"first_name": strings.TrimSpace(c.PostForm("first_name")), "last_name": strings.TrimSpace(c.PostForm("last_name"))}})
			}
			if err == nil {
				if nu, e := s.store.UserByID(c, newID); e == nil {
					s.pluginUserAction(c, "user_register", nu)
					s.notifyNewUser(c, nu, c.PostForm("send_user_notification") == "1")
					s.asAudit(c, asEvent{Event: "user_created", Severity: "notice", Object: nu.Login, Message: "User " + nu.Login + " created with the role " + role + "."})
				}
				c.Redirect(http.StatusFound, "/wp-admin/users?update=add")
				return
			}
			errs = append(errs, "<strong>Error:</strong> "+template.HTMLEscapeString(err.Error()))
		}
		d.Error = template.HTML(strings.Join(errs, "<br />"))
	}
	if d.Values.Get("role") == "" {
		d.Values.Set("role", d.Def)
	}
	s.adminPage(c, "user-new", "Add User", "user-new-php", "admin/user-new", d)
}

// userAccessKey is a user's roles and per-user capability exceptions as one comparable string.
func (s *Server) userAccessKey(ctx context.Context, userID int64) string {
	var k string
	_ = s.store.Pool.QueryRow(ctx, `SELECT
		coalesce((SELECT string_agg(r.slug || ':' || ur.is_primary::text, ',' ORDER BY r.slug) FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = $1), '') || '|' ||
		coalesce((SELECT string_agg(capability || ':' || effect, ',' ORDER BY capability) FROM user_capabilities WHERE user_id = $1), '')`, userID).Scan(&k)
	return k
}

// endSessionsAfterAccessChange ends the user's sessions when their roles or capability exceptions
// differ from before (userAccessKey): a session opened under the old role must not keep running, and
// a stolen session must not profit from a promotion. The acting user's own current session is kept
// when they edit themselves (endSessionsAfterPasswordChange). WordPress keeps sessions here; Pursae
// ends them on purpose.
func (s *Server) endSessionsAfterAccessChange(ctx context.Context, c *gin.Context, userID int64, before string) {
	if s.userAccessKey(ctx, userID) != before {
		s.endSessionsAfterPasswordChange(ctx, c, userID)
	}
}
