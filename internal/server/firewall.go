package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"wpgo/internal/config"
)

// The core firewall: a per-IP request rate limit and a few request rules (a basic WAF). It is on
// by default, before Pursae Security and the page cache, so every site gets it without a plugin.
// Pursae Security (Settings › Firewall) can change the limit or turn either part off; with the
// plugin off the core defaults apply. PURSAE_FIREWALL=off switches both off for the process.
//
// Only the URL (path and query) and the User-Agent are inspected, never a request body: post
// content, block markup and code in the Theme File Editor are legitimate input there.
//
// Addresses that are loopback or a trusted proxy (PURSAE_TRUSTED_PROXIES) are never rate limited:
// without a forwarded header they are the proxy itself, which carries every visitor.

// fwDefaultRateLimit is the core default: requests per minute from one IP address. Static files
// do not count, so this is pages, REST calls and form posts; the block editor of a busy author
// makes a few hundred a minute at most.
const fwDefaultRateLimit = 900

// fwMaxTracked caps the addresses counted in one minute (a flood from many addresses cannot grow
// the map without bound; addresses past the cap are not limited in that minute).
const fwMaxTracked = 200_000

type fwLimiter struct {
	mu     sync.Mutex
	minute int64
	counts map[string]int
	logged map[string]bool // one activity-log row per address and minute
}

func (l *fwLimiter) hit(ip string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := now.Unix() / 60
	if m != l.minute || l.counts == nil {
		l.minute, l.counts, l.logged = m, map[string]int{}, map[string]bool{}
	}
	n, ok := l.counts[ip]
	if !ok && len(l.counts) >= fwMaxTracked {
		return 0
	}
	n++
	l.counts[ip] = n
	return n
}

// firstLog reports whether this is the address's first log row in the current minute.
func (l *fwLimiter) firstLog(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logged == nil {
		l.logged = map[string]bool{}
	}
	if l.logged[key] || len(l.logged) >= 1000 {
		return false
	}
	l.logged[key] = true
	return true
}

var fwLimiters sync.Map // *Server -> *fwLimiter

func (s *Server) fwLimiter() *fwLimiter {
	v, _ := fwLimiters.LoadOrStore(s, &fwLimiter{})
	return v.(*fwLimiter)
}

// fwConfig is the firewall's effective configuration.
type fwConfig struct {
	RateLimit int  // requests per minute and IP; 0: off
	Rules     bool // the WAF rules
	AllowList string
}

// fwEnvOff reports PURSAE_FIREWALL=off.
func fwEnvOff() bool {
	v := strings.ToLower(strings.TrimSpace(config.Env("FIREWALL")))
	return v == "off" || v == "0" || v == "false"
}

func (s *Server) fwConfig() fwConfig {
	if fwEnvOff() {
		return fwConfig{}
	}
	if s.adminsecOn() {
		st := s.asLoad()
		return fwConfig{RateLimit: st.RateLimit, Rules: st.Firewall, AllowList: st.AllowList}
	}
	return fwConfig{RateLimit: fwDefaultRateLimit, Rules: true}
}

// fwStaticExt are file types that never count toward the rate limit.
var fwStaticExt = map[string]bool{".css": true, ".js": true, ".mjs": true, ".map": true, ".png": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".webp": true, ".avif": true, ".svg": true, ".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".eot": true, ".mp4": true, ".webm": true, ".mp3": true, ".ogg": true, ".wav": true, ".pdf": true}

// fwStaticRequest reports requests that never count toward the rate limit: GET/HEAD for the asset
// trees, or for a static file type elsewhere only without a query string, because the site router
// and plugin hooks answer any path (/x.css?s=term, POST /x.css?wc-ajax=...) dynamically.
func fwStaticRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/wp-includes/") || strings.HasPrefix(p, "/wp-content/") || strings.HasPrefix(p, "/wp-admin/assets/") ||
		strings.HasPrefix(p, "/wp-admin/css/") || strings.HasPrefix(p, "/wp-admin/js/") || strings.HasPrefix(p, "/wp-admin/images/") {
		return true
	}
	return r.URL.RawQuery == "" && fwStaticExt[strings.ToLower(path.Ext(p))]
}

// fwRule is one WAF rule: a name for the log and the pattern it looks for.
type fwRule struct {
	Name string
	re   *regexp.Regexp
}

// fwPathRules look at the decoded path: probes for files a site must never serve.
var fwPathRules = []fwRule{
	{"path traversal", regexp.MustCompile(`(^|[/\\])\.\.([/\\]|$)`)},
	{"null byte", regexp.MustCompile(`\x00`)},
	{"sensitive file probe", regexp.MustCompile(`(?i)(^|/)(\.env(\.[a-z0-9_-]+)?|\.git(/|$)|\.svn(/|$)|\.hg(/|$)|\.htaccess|\.htpasswd|\.ds_store|wp-config\.php(\.[a-z~]+)?|web\.config|composer\.(json|lock)|id_rsa)$|(^|/)\.git/`)},
	{"system file probe", regexp.MustCompile(`(?i)/etc/(passwd|shadow|hosts)|/proc/self/`)},
}

// fwQueryRules look at the decoded query string (and fwUARules at the User-Agent).
var fwQueryRules = []fwRule{
	{"path traversal", regexp.MustCompile(`(\.\./|\.\.\\){2,}|/etc/passwd|/proc/self/environ`)},
	{"null byte", regexp.MustCompile(`\x00`)},
	{"script injection", regexp.MustCompile(`(?i)<\s*script[\s>/]|javascript\s*:\s*[a-z_$.]+\s*\(|<[^>]+\son(error|load|mouseover|focus)\s*=`)},
	{"SQL injection", regexp.MustCompile(`(?i)\bunion\b(\s|/\*.*?\*/|\+)+(all(\s|/\*.*?\*/|\+)+)?select\b|\binformation_schema\b|\bpg_sleep\s*\(|\bsleep\s*\(\s*\d+\s*\)|\bbenchmark\s*\(\s*\d+|\bwaitfor\s+delay\b`)},
	{"remote code injection", regexp.MustCompile(`(?i)\$\{jndi:|\$\{\$\{|<\?php|\b(base64_decode|eval|system|passthru|shell_exec)\s*\(|;\s*(wget|curl)\s+https?://`)},
}

var fwUARules = []fwRule{
	{"remote code injection", regexp.MustCompile(`(?i)\$\{jndi:|\$\{\$\{|\(\)\s*\{\s*:;\s*\};`)},
}

// fwMatch returns the name of the first rule the request breaks ("" when it is clean).
func fwMatch(r *http.Request) string {
	p := r.URL.Path
	if dp, err := url.PathUnescape(p); err == nil { // a path is decoded already; %2e%2e / %00 in RawPath too
		p = dp
	}
	if r.URL.RawPath != "" {
		if dp, err := url.PathUnescape(r.URL.RawPath); err == nil {
			p += "\n" + dp
		}
	}
	for _, rule := range fwPathRules {
		if rule.re.MatchString(p) {
			return rule.Name
		}
	}
	if q := r.URL.RawQuery; q != "" {
		dq, err := url.QueryUnescape(q)
		if err != nil {
			dq = q
		}
		// Decode twice: "%253Cscript" is "<script" once a second decoder sees it.
		if d2, err := url.QueryUnescape(dq); err == nil {
			dq += "\n" + d2
		}
		for _, rule := range fwQueryRules {
			if rule.re.MatchString(dq) {
				return rule.Name
			}
		}
	}
	if ua := r.UserAgent(); ua != "" {
		for _, rule := range fwUARules {
			if rule.re.MatchString(ua) {
				return rule.Name
			}
		}
	}
	return ""
}

// firewallServe runs first in Server.Handler. It reports whether it answered the request (403
// for a rule, 429 over the rate limit).
func (s *Server) firewallServe(w http.ResponseWriter, r *http.Request) bool {
	cfg := s.fwConfig()
	if cfg.RateLimit <= 0 && !cfg.Rules {
		return false
	}
	ip := s.requestIP(r)
	if cfg.AllowList != "" && maintIPAllowed(cfg.AllowList, ip) {
		return false
	}
	if cfg.Rules {
		if rule := fwMatch(r); rule != "" {
			s.fwLog(r, ip, "firewall_blocked", "Request blocked by the firewall ("+rule+"): "+r.Method+" "+fwTrim(r.URL.RequestURI(), 300))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("403 Forbidden: the request was blocked by the site's firewall.\n"))
			return true
		}
	}
	if cfg.RateLimit > 0 && !fwStaticRequest(r) && r.URL.Path != cspReportPath && !s.fwExempt(ip) { // CSP reports have their own limit
		if n := s.fwLimiter().hit(ip, time.Now()); n > cfg.RateLimit {
			s.fwLog(r, ip, "rate_limited", "More than "+strconv.Itoa(cfg.RateLimit)+" requests a minute from this IP address; answering 429 until the minute ends.")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", strconv.Itoa(int(60-time.Now().Unix()%60)))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("429 Too Many Requests: please slow down and try again in a minute.\n"))
			return true
		}
	}
	return false
}

// fwExempt: loopback and trusted proxies are infrastructure, not visitors.
func (s *Server) fwExempt(ip string) bool {
	p := net.ParseIP(ip)
	return p == nil || p.IsLoopback() || s.isTrustedProxy(p)
}

// fwLog writes one Pursae Security activity-log row per address, event and minute (and a log line).
func (s *Server) fwLog(r *http.Request, ip, event, msg string) {
	if !s.fwLimiter().firstLog(event + "|" + ip) {
		return
	}
	slog.Warn("firewall", "event", event, "ip", ip, "msg", msg)
	if s.adminsecOn() {
		ua := r.UserAgent()
		ev := asEvent{Event: event, Severity: "warning", IP: ip, Object: fwTrim(ua, 200), Message: msg}
		safeGo("firewall.fwLog", func() { s.asAudit(context.Background(), ev) })
	}
}

func fwTrim(v string, n int) string {
	if len(v) > n {
		return v[:n] + "…"
	}
	return v
}
