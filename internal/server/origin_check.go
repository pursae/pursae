package server

import (
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// crossSitePost reports whether a browser says this request came from another site: Sec-Fetch-Site is
// "cross-site", or an Origin header (Referer when there is none, with analytics) names a host that is
// neither this request's Host nor the configured home's. "null" (a sandboxed frame, a data: page) is
// foreign. A request without those headers (an old client, curl) is not refused: they are a defence on
// top of nonces and SameSite cookies, for the forms that have no nonce (wp-login, wp-comments-post).
func (s *Server) crossSitePost(c *gin.Context) bool {
	if strings.EqualFold(c.GetHeader("Sec-Fetch-Site"), "cross-site") {
		return true
	}
	o := strings.TrimSpace(c.GetHeader("Origin"))
	if o == "" {
		return false
	}
	return !s.siteHostIs(c, o)
}

// siteHostIs reports whether the URL raw (an Origin or Referer) is on this site: its host[:port] is the
// request's Host or the configured home's host.
func (s *Server) siteHostIs(c *gin.Context, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, c.Request.Host) {
		return true
	}
	if h, err := url.Parse(s.configuredHome()); err == nil && h.Host != "" && strings.EqualFold(u.Host, h.Host) {
		return true
	}
	return false
}
