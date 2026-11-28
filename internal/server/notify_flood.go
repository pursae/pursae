package server

import (
	"log/slog"
	"net"
	"strings"
	"time"
)

// Notification e-mails a visitor can trigger (a comment waiting for moderation, a comment on an author's
// post, a contact form submission) are capped, so a script cannot turn the forms into a mail bomb aimed
// at the site's owner: per source network and per site, in a sliding window. What the visitor sent is
// still saved (the comment, the form entry); only the e-mail is left out, and the first one left out in
// a window is logged.
const (
	notifyMailWindow    = 10 * time.Minute
	notifyMailPerSource = 5  // per IPv4 /24 or IPv6 /64
	notifyMailPerSite   = 30 // all sources together
)

// notifyMailAllowed reports whether a notification of kind ("comment", "contact") caused by a visitor at
// ip may be mailed now, and counts it.
func (s *Server) notifyMailAllowed(kind, ip string) bool {
	site := s.configuredHome()
	src := notifySourceNet(ip)
	if forumAllow("notify-src:"+site+"|"+src, notifyMailPerSource, notifyMailWindow) &&
		forumAllow("notify-site:"+site, notifyMailPerSite, notifyMailWindow) {
		return true
	}
	if forumAllow("notify-logged:"+site+"|"+kind, 1, notifyMailWindow) {
		slog.Warn("notification e-mails capped: the submissions are saved but not mailed", "kind", kind, "source", src,
			"per_source", notifyMailPerSource, "per_site", notifyMailPerSite, "window", notifyMailWindow.String())
	}
	return false
}

// notifySourceNet is the network a visitor's notifications are counted under: an IPv4 /24, an IPv6 /64.
func notifySourceNet(addr string) string {
	ip := net.ParseIP(strings.TrimSpace(addr))
	if ip == nil {
		return addr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}
