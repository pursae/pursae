package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Front-end members: forum participants, subscribers and store customers. Their roles grant nothing a
// wp-admin screen serves (only read, level_0 and forum_participate), so they get neither the dashboard
// nor Application Passwords — the forum's account page and the store's My Account are where they manage
// themselves. This is always on, like WooCommerce's prevent-admin-access for customers: a site full of
// self-registered forum members must not hand each of them API credentials or an admin entry point.

// frontEndCaps are the capabilities a front-end member's role holds. A user with any other capability
// (edit_posts, forum_moderate, upload_files, a plugin's own…) keeps wp-admin.
var frontEndCaps = map[string]bool{"read": true, "level_0": true, "forum_participate": true}

// frontEndOnly reports whether u is a front-end member (see above).
func frontEndOnly(u *store.User) bool {
	if u == nil {
		return true
	}
	for c, ok := range u.Caps {
		if ok && !frontEndCaps[c] {
			return false
		}
	}
	return true
}

var errAppPasswordsUnavailable = errors.New("Application passwords are not available for your account. Please contact the site administrator for assistance.")

// appPasswordsAvailable is wp_is_application_passwords_available_for_user().
func appPasswordsAvailable(u *store.User) bool { return !frontEndOnly(u) }

// adminOpenToFrontEnd are the wp-admin paths a front-end member still reaches: admin-ajax serves the
// front end (WordPress lets every visitor call it).
func adminOpenToFrontEnd(p string) bool {
	p = strings.TrimSuffix(p, "/")
	return p == "/wp-admin/admin-ajax" || p == "/wp-admin/admin-ajax.php"
}

// emailChangeLink reports whether the request is the confirmation link of an e-mail change
// (sendEmailChangeConfirmation): front-end members change their address from My Account and confirm
// it there; profile() answers it with a redirect back to the front end and never shows the screen.
func emailChangeLink(c *gin.Context) bool {
	p := strings.TrimSuffix(c.Request.URL.Path, "/")
	return (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) &&
		(p == "/wp-admin/profile" || p == "/wp-admin/profile.php") && c.Query("newuseremail") != ""
}

// frontEndHome is where a front-end member goes instead of wp-admin: the forum account page while
// Pursae Forum is on, otherwise the home page.
func (s *Server) frontEndHome(c *gin.Context) string {
	if s.forumOn() {
		return s.forumURL(c, "account")
	}
	return s.links(c).HomeURL("/")
}

// denyFrontEndAdmin answers a front-end member's wp-admin request: a redirect for a page view, 403 for
// anything else. It reports whether the request was refused.
func (s *Server) denyFrontEndAdmin(c *gin.Context, u *store.User) bool {
	if !frontEndOnly(u) || adminOpenToFrontEnd(c.Request.URL.Path) || emailChangeLink(c) {
		return false
	}
	if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
		c.Redirect(http.StatusFound, s.frontEndHome(c))
	} else {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
	}
	c.Abort()
	return true
}
