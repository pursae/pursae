package server

import (
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
	action := c.Query("action")
	if action == "" || action == "-1" {
		action = c.Query("action2")
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
	r := c.Query("new_role")
	if c.Query("changeit2") != "" { // bottom "Change role to…" control
		r = c.Query("new_role2")
	}
	if r != "" && (c.Query("changeit") != "" || c.Query("changeit2") != "") {
		if !s.checkNonce(c, "bulk-users") || !u.Can("promote_users", nil) {
			s.denyNonce(c)
			return
		}
		for _, raw := range c.QueryArray("users[]") {
			id, _ := strconv.ParseInt(raw, 10, 64)
			if id != u.ID {
				_ = s.store.SetUserRoles(ctx, id, r, nil)
			}
		}
		c.Redirect(http.StatusFound, "/wp-admin/users?update=promote")
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
	d := usersListData{Role: q.Role, Search: q.Search, BulkNonce: s.nonce(c, "bulk-users"), Roles: roles,
		SortLinks: sortLinks(c, map[string]bool{"login": false, "email": false}, "login", "asc")}
	d.Views = append(d.Views, roleView{Slug: "", Name: "All", Count: counts["all"], Current: q.Role == ""})
	for _, r := range roles {
		if counts[r.Slug] > 0 {
			d.Views = append(d.Views, roleView{Slug: r.Slug, Name: r.Name, Count: counts[r.Slug], Current: q.Role == r.Slug})
		}
	}
	var ids []int64
	for _, x := range list {
		ids = append(ids, x.ID)
	}
	postCounts, _ := s.store.UserPostCounts(ctx, ids)
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
	if v := c.Query("user"); v != "" {
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
	if !u.Can("delete_users", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to delete users.")
		return
	}
	if !s.checkNonce(c, "delete-users") {
		s.denyNonce(c)
		return
	}
	var reassign int64
	if c.Query("delete_option") == "reassign" {
		reassign, _ = strconv.ParseInt(c.Query("reassign_user"), 10, 64)
	}
	for _, id := range selectedUserIDs(c) {
		if id == u.ID || id == reassign {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to delete this user.")
			return
		}
		if err := s.store.DeleteUser(c, id, reassign); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
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
}

// userNew is user-new.php.
func (s *Server) userNew(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("create_users", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to create users.")
		return
	}
	roles, _ := s.store.Roles(c)
	d := userNewData{Nonce: s.nonce(c, "create-user"), Roles: roles, Def: s.settings.String("default_role"), Values: url.Values{}}
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
		var errs []string
		if login == "" {
			errs = append(errs, "<strong>Error:</strong> Please enter a username.")
		}
		if _, err := mail.ParseAddress(email); err != nil || email == "" {
			errs = append(errs, "<strong>Error:</strong> The email address is not correct.")
		}
		if pass == "" {
			errs = append(errs, "<strong>Error:</strong> Please enter a password.")
		}
		if lt, et, _ := s.store.LoginOrEmailTaken(c, login, email); lt {
			errs = append(errs, "<strong>Error:</strong> This username is already registered. Please choose another one.")
		} else if et {
			errs = append(errs, "<strong>Error:</strong> This email is already registered. Please choose another one.")
		}
		if role == "administrator" && !u.Can("promote_users", nil) {
			role = d.Def
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
					s.notifyNewUser(c, nu, c.PostForm("send_user_notification") == "1")
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
