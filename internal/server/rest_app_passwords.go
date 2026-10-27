package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// WP_REST_Application_Passwords_Controller: /wp/v2/users/{user_id}/application-passwords.

func (api *restAPI) registerAppPasswords() {
	base := `/wp/v2/users/(?P<user_id>(?:[\d]+|me))/application-passwords`
	api.register("wp/v2", base, map[string]restHandler{
		http.MethodGet:    api.listAppPasswords,
		http.MethodPost:   api.createAppPassword,
		http.MethodDelete: api.deleteAppPasswords,
	}, nil)
	api.register("wp/v2", base+"/introspect", map[string]restHandler{http.MethodGet: api.introspectAppPassword}, nil)
	api.register("wp/v2", base+`/(?P<uuid>[\w\-]+)`, map[string]restHandler{
		http.MethodGet:    api.getAppPassword,
		http.MethodPost:   api.updateAppPassword,
		http.MethodPut:    api.updateAppPassword,
		http.MethodDelete: api.deleteAppPassword,
	}, nil)
}

func (s *Server) restAppPassword(c *gin.Context, u *store.User, ap *appPassword, plain string) map[string]any {
	out := map[string]any{"uuid": ap.UUID, "app_id": ap.AppID, "name": ap.Name, "created": gmt(ap.CreatedAt), "last_used": nil, "last_ip": nil}
	if ap.LastUsed != nil {
		out["last_used"] = gmt(*ap.LastUsed)
	}
	if ap.LastIP != "" {
		out["last_ip"] = ap.LastIP
	}
	if plain != "" {
		out["password"] = plain
	}
	out["_links"] = map[string]any{"self": []map[string]any{{"href": s.links(c).HomeURL("/wp-json/wp/v2/users/" + itoa64(u.ID) + "/application-passwords/" + ap.UUID)}}}
	return out
}

// appPasswordUser resolves {user_id} ("me" or an ID) and checks edit_user: WP's permission callbacks for
// every route of the controller.
func (api *restAPI) appPasswordUser(c *gin.Context, r *restRequest, action string) (*store.User, bool) {
	s := api.s
	cur := restCurrent(c)
	msg := map[string]string{
		"list":   "Sorry, you are not allowed to list application passwords for this user.",
		"create": "Sorry, you are not allowed to create application passwords for this user.",
		"read":   "Sorry, you are not allowed to read this application password.",
		"edit":   "Sorry, you are not allowed to edit this application password.",
		"delete": "Sorry, you are not allowed to delete this application password.",
		"revoke": "Sorry, you are not allowed to delete application passwords for this user.",
	}[action]
	code := map[string]string{"list": "rest_cannot_list_application_passwords", "create": "rest_cannot_create_application_passwords",
		"read": "rest_cannot_read_application_password", "edit": "rest_cannot_edit_application_password",
		"delete": "rest_cannot_delete_application_password", "revoke": "rest_cannot_delete_application_passwords"}[action]
	var u *store.User
	if r.Params["user_id"] == "me" {
		if cur == nil {
			restErr(c, http.StatusUnauthorized, "rest_not_logged_in", "You are not currently logged in.")
			return nil, false
		}
		u = cur
	} else {
		id, _ := strconv.ParseInt(r.Params["user_id"], 10, 64)
		var err error
		if u, err = s.store.UserByID(c, id); err != nil {
			restErr(c, http.StatusNotFound, "rest_user_invalid_id", "Invalid user ID.")
			return nil, false
		}
	}
	if cur == nil {
		restErr(c, http.StatusUnauthorized, code, msg)
		return nil, false
	}
	if cur.ID != u.ID && !cur.Can("edit_users", nil) {
		restErr(c, http.StatusForbidden, code, msg)
		return nil, false
	}
	return u, true
}

func (api *restAPI) listAppPasswords(c *gin.Context, r *restRequest) {
	u, ok := api.appPasswordUser(c, r, "list")
	if !ok {
		return
	}
	list, err := api.s.appPasswords(c, u.ID)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_query_error", err.Error())
		return
	}
	out := []map[string]any{}
	for _, ap := range list {
		out = append(out, api.s.restAppPassword(c, u, ap, ""))
	}
	restJSON(c, http.StatusOK, out)
}

func (api *restAPI) createAppPassword(c *gin.Context, r *restRequest) {
	u, ok := api.appPasswordUser(c, r, "create")
	if !ok {
		return
	}
	name, has := r.Str(c, "name")
	if !has {
		restErr(c, http.StatusBadRequest, "rest_missing_callback_param", "Missing parameter(s): name", map[string]any{"params": []string{"name"}})
		return
	}
	appID, _ := r.Str(c, "app_id")
	plain, ap, err := api.s.createAppPassword(c, u.ID, name, appID)
	if err != nil {
		code := "application_password_empty_name"
		if err == errAppPasswordDuplicateName {
			code = "application_password_duplicate_name"
		} else if err != errAppPasswordEmptyName {
			code = "rest_invalid_param"
		}
		restErr(c, http.StatusBadRequest, code, err.Error())
		return
	}
	c.Header("Location", api.s.links(c).HomeURL("/wp-json/wp/v2/users/"+itoa64(u.ID)+"/application-passwords/"+ap.UUID))
	restJSON(c, http.StatusCreated, api.s.restAppPassword(c, u, ap, plain))
}

func (api *restAPI) getAppPassword(c *gin.Context, r *restRequest) {
	u, ok := api.appPasswordUser(c, r, "read")
	if !ok {
		return
	}
	ap, err := api.s.appPasswordByUUID(c, u.ID, r.Params["uuid"])
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_application_password_not_found", "Application password not found.")
		return
	}
	restJSON(c, http.StatusOK, api.s.restAppPassword(c, u, ap, ""))
}

func (api *restAPI) updateAppPassword(c *gin.Context, r *restRequest) {
	u, ok := api.appPasswordUser(c, r, "edit")
	if !ok {
		return
	}
	ap, err := api.s.appPasswordByUUID(c, u.ID, r.Params["uuid"])
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_application_password_not_found", "Application password not found.")
		return
	}
	if name, has := r.Str(c, "name"); has {
		if err := api.s.renameAppPassword(c, ap, name); err != nil {
			restErr(c, http.StatusBadRequest, "application_password_empty_name", err.Error())
			return
		}
	}
	restJSON(c, http.StatusOK, api.s.restAppPassword(c, u, ap, ""))
}

func (api *restAPI) deleteAppPassword(c *gin.Context, r *restRequest) {
	u, ok := api.appPasswordUser(c, r, "delete")
	if !ok {
		return
	}
	ap, err := api.s.appPasswordByUUID(c, u.ID, r.Params["uuid"])
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_application_password_not_found", "Application password not found.")
		return
	}
	prev := api.s.restAppPassword(c, u, ap, "")
	if err := api.s.deleteAppPassword(c, ap.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The application password cannot be deleted.")
		return
	}
	restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
}

func (api *restAPI) deleteAppPasswords(c *gin.Context, r *restRequest) {
	u, ok := api.appPasswordUser(c, r, "revoke")
	if !ok {
		return
	}
	n, err := api.s.deleteAllAppPasswords(c, u.ID)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The application passwords cannot be deleted.")
		return
	}
	restJSON(c, http.StatusOK, map[string]any{"deleted": true, "count": n})
}

// introspectAppPassword returns the application password that authenticated this request.
func (api *restAPI) introspectAppPassword(c *gin.Context, r *restRequest) {
	u, ok := api.appPasswordUser(c, r, "read")
	if !ok {
		return
	}
	id, has := c.Get(restAppPasswordKey)
	if !has {
		restErr(c, http.StatusNotFound, "rest_no_authenticated_app_password", "Cannot introspect application password.")
		return
	}
	ap, err := api.s.appPasswordByID(c, id.(int64))
	if err != nil || ap == nil {
		restErr(c, http.StatusNotFound, "rest_no_authenticated_app_password", "Cannot introspect application password.")
		return
	}
	if u.ID != restCurrent(c).ID {
		restErr(c, http.StatusNotFound, "rest_no_authenticated_app_password", "Cannot introspect application password.")
		return
	}
	now := time.Now()
	ap.LastUsed, ap.LastIP = &now, clientIP(c)
	restJSON(c, http.StatusOK, api.s.restAppPassword(c, u, ap, ""))
}
