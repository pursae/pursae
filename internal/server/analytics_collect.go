package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
)

// The collector of Pursae Analytics (analytics.go): POST /wpgo-analytics/collect with a small JSON
// body sent by navigator.sendBeacon. It always answers 204 (a beacon reads nothing), drops bots,
// Do Not Track / GPC requests (when respected), excluded addresses and signed-in users (when
// excluded), and limits every address to a few hundred requests a minute.

// analyticsHit is the beacon's body.
type analyticsHit struct {
	T    string   `json:"t"`  // v: page view, e: event, x: the view ends (visible time, Web Vitals)
	ID   float64  `json:"id"` // the view's random id
	P    string   `json:"p"`  // path and query of the page
	R    string   `json:"r"`  // document.referrer
	W    int      `json:"w"`  // viewport width
	N    string   `json:"n"`  // event name
	G    string   `json:"g"`  // event target
	D    float64  `json:"d"`  // visible time, ms
	LCP  *float64 `json:"lcp"`
	INP  *float64 `json:"inp"`
	CLS  *float64 `json:"cls"`
	TTFB *float64 `json:"ttfb"`
	FCP  *float64 `json:"fcp"`
}

var analyticsEventNames = map[string]bool{"outbound": true, "download": true, "form": true, "404": true}

var analyticsBotRe = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|headless|lighthouse|pagespeed|preview|facebookexternalhit|curl|wget|python|go-http|java/|okhttp|monitor|uptime|pingdom`)

func (s *Server) analyticsCollect(c *gin.Context) {
	c.Status(http.StatusNoContent)
	r := c.Request
	ua := r.UserAgent()
	if ua == "" || analyticsBotRe.MatchString(ua) || r.Header.Get("Purpose") == "prefetch" || r.Header.Get("Sec-Purpose") != "" {
		return
	}
	// The beacon is sent by this site's pages: a browser on another site's page (Origin, else Referer)
	// is someone filling the statistics with made-up visits.
	if o := r.Header.Get("Origin"); o != "" {
		if !s.siteHostIs(c, o) {
			return
		}
	} else if ref := r.Header.Get("Referer"); ref != "" && !s.siteHostIs(c, ref) {
		return
	}
	st := s.analyticsLoad()
	if st.RespectDNT && (r.Header.Get("DNT") == "1" || r.Header.Get("Sec-GPC") == "1") {
		return
	}
	if st.ExcludeLoggedIn {
		if ck, err := r.Cookie(auth.CookieName); err == nil && ck.Value != "" {
			return
		}
	}
	ip := s.requestIP(r)
	if analyticsIPExcluded(st.ExcludeIPs, ip) || !analyticsLimit.allow(wbFormIPKey(ip), time.Now()) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		return
	}
	var h analyticsHit
	if json.Unmarshal(raw, &h) != nil || h.ID < 1 || h.ID > 9.1e15 {
		return
	}
	u, err := url.Parse(h.P)
	if err != nil || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "/wp-admin") {
		return
	}
	ctx := r.Context()
	// The site's configured host, not the request's Host: a client could otherwise become a new
	// visitor per request by changing the header.
	visitor, err := s.analyticsVisitor(ctx, ip, ua, s.analyticsSiteHost())
	if err != nil {
		return
	}
	pageID := int64(h.ID)
	path := analyticsTrim(u.Path, 512)
	switch h.T {
	case "v":
		if !forumAllow("analytics-view:"+wbFormIPKey(ip), analyticsViewsPerMinute, time.Minute) {
			return
		}
		q := u.Query()
		device, browser, osName := analyticsUA(ua, h.W)
		_, _ = s.store.Pool.Exec(ctx, `INSERT INTO analytics_events (visitor, page_id, name, path, referrer, utm_source, utm_medium,
			utm_campaign, country, device, browser, os) VALUES ($1,$2,'pageview',$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			visitor, pageID, path, s.analyticsReferrer(h.R, r.Host), analyticsTrim(strings.ToLower(q.Get("utm_source")), 100),
			analyticsTrim(strings.ToLower(q.Get("utm_medium")), 100), analyticsTrim(q.Get("utm_campaign"), 150),
			s.visitorCountry(r, ip), device, browser, osName)
	case "e":
		if !analyticsEventNames[h.N] {
			return
		}
		target := analyticsTrim(h.G, 512)
		if h.N == "404" {
			target = path
		}
		device, browser, osName := analyticsUA(ua, h.W)
		_, _ = s.store.Pool.Exec(ctx, `INSERT INTO analytics_events (visitor, page_id, name, path, target, country, device, browser, os)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, visitor, pageID, h.N, path, target, s.visitorCountry(r, ip), device, browser, osName)
	case "x":
		_, _ = s.store.Pool.Exec(ctx, `UPDATE analytics_events SET engaged_ms = GREATEST(engaged_ms, $3),
			lcp_ms = COALESCE($4, lcp_ms), inp_ms = COALESCE($5, inp_ms), cls = COALESCE($6, cls),
			ttfb_ms = COALESCE($7, ttfb_ms), fcp_ms = COALESCE($8, fcp_ms)
			WHERE page_id = $1 AND visitor = $2 AND name = 'pageview' AND ts > now() - interval '1 day'`,
			pageID, visitor, int(min(max(h.D, 0), 30*60*1000)), analyticsMs(h.LCP), analyticsMs(h.INP),
			analyticsCLS(h.CLS), analyticsMs(h.TTFB), analyticsMs(h.FCP))
	}
}

// analyticsViewsPerMinute caps the page views one address records a minute: far above a reader's pace,
// low enough that one machine cannot invent a crowd.
const analyticsViewsPerMinute = 60

// analyticsSiteHost is the configured home's host, the site part of the visitor hash.
func (s *Server) analyticsSiteHost() string {
	if u, err := url.Parse(s.configuredHome()); err == nil {
		return u.Host
	}
	return ""
}

// analyticsMs is a timing in ms for the database: nil when missing or out of range (0–60 s).
func analyticsMs(v *float64) any {
	if v == nil || *v < 0 || *v > 60000 {
		return nil
	}
	return int(*v + 0.5)
}

func analyticsCLS(v *float64) any {
	if v == nil || *v < 0 || *v > 10 {
		return nil
	}
	return float32(*v)
}

func analyticsTrim(v string, n int) string {
	v = strings.TrimSpace(strings.ToValidUTF8(v, ""))
	if len(v) > n {
		v = v[:n]
		for len(v) > 0 && !utf8Start(v[len(v)-1]) {
			v = v[:len(v)-1]
		}
	}
	return v
}

func utf8Start(b byte) bool { return b < 0x80 || b >= 0xC0 }

// analyticsVisitor is the visitor's hash for today: SHA-256 of the day's salt, the address, the
// user agent and the site's host, cut to a positive int64.
func (s *Server) analyticsVisitor(ctx context.Context, ip, ua, host string) (int64, error) {
	salt, err := s.analyticsSalt(ctx)
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte("\x00" + ip + "\x00" + ua + "\x00" + strings.ToLower(host)))
	return int64(binary.BigEndian.Uint64(h.Sum(nil)[:8]) &^ (1 << 63)), nil
}

// analyticsSalts caches the day's salt; every instance reads the same row (the first insert of the
// day wins).
var analyticsSalts struct {
	sync.Mutex
	day  string
	salt []byte
}

func (s *Server) analyticsSalt(ctx context.Context) ([]byte, error) {
	day := time.Now().UTC().Format("2006-01-02")
	analyticsSalts.Lock()
	defer analyticsSalts.Unlock()
	if analyticsSalts.day == day {
		return analyticsSalts.salt, nil
	}
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	var salt []byte
	if err := s.store.Pool.QueryRow(ctx, `INSERT INTO analytics_salts (day, salt) VALUES ($1::date, $2)
		ON CONFLICT (day) DO UPDATE SET day = EXCLUDED.day RETURNING salt`, day, fresh).Scan(&salt); err != nil {
		return nil, err
	}
	// Yesterday's salt goes now: its hashes can never be recomputed.
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM analytics_salts WHERE day < $1::date`, day)
	analyticsSalts.day, analyticsSalts.salt = day, salt
	return salt, nil
}

// analyticsReferrer is the source of a view: ” for direct and internal visits, a known service's
// name, or the referring host without "www.".
func (s *Server) analyticsReferrer(ref, host string) string {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil || u.Host == "" {
		if strings.HasPrefix(ref, "android-app://") {
			return analyticsSourceName(strings.TrimPrefix(ref, "android-app://"))
		}
		return ""
	}
	h := strings.ToLower(u.Hostname())
	own := strings.ToLower(host)
	if hh, _, err := net.SplitHostPort(own); err == nil {
		own = hh
	}
	if h == own || strings.TrimPrefix(h, "www.") == strings.TrimPrefix(own, "www.") {
		return ""
	}
	if home, err := url.Parse(s.settings.String("home")); err == nil && strings.EqualFold(home.Hostname(), h) {
		return ""
	}
	return analyticsSourceName(h)
}

// analyticsHosts and analyticsLabels name well-known sources: a whole host, or a domain label
// anywhere in it (google.com.tr, news.google.com and l.facebook.com are Google and Facebook).
var analyticsHosts = map[string]string{"t.co": "X (Twitter)", "x.com": "X (Twitter)", "fb.com": "Facebook", "fb.me": "Facebook",
	"lnkd.in": "LinkedIn", "claude.ai": "Claude", "gemini.google.com": "Gemini", "copilot.microsoft.com": "Copilot",
	"news.ycombinator.com": "Hacker News", "search.brave.com": "Brave Search", "youtu.be": "YouTube"}

var analyticsLabels = map[string]string{"google": "Google", "bing": "Bing", "duckduckgo": "DuckDuckGo", "yandex": "Yandex",
	"yahoo": "Yahoo", "baidu": "Baidu", "ecosia": "Ecosia", "chatgpt": "ChatGPT", "openai": "ChatGPT", "perplexity": "Perplexity",
	"facebook": "Facebook", "instagram": "Instagram", "twitter": "X (Twitter)", "linkedin": "LinkedIn", "youtube": "YouTube",
	"reddit": "Reddit", "pinterest": "Pinterest", "tiktok": "TikTok", "whatsapp": "WhatsApp", "telegram": "Telegram", "github": "GitHub"}

func analyticsSourceName(host string) string {
	h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(host)), "www.")
	if n, ok := analyticsHosts[h]; ok {
		return n
	}
	for _, label := range strings.Split(h, ".") {
		if n, ok := analyticsLabels[label]; ok {
			return n
		}
	}
	return analyticsTrim(h, 100)
}

// analyticsUA is the device class, browser and operating system of a user agent (the viewport
// width decides between phones, tablets and desktops when the user agent does not tell).
func analyticsUA(ua string, width int) (device, browser, osName string) {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "ipad") || strings.Contains(l, "tablet") || (strings.Contains(l, "android") && !strings.Contains(l, "mobile")):
		device = "Tablet"
	case strings.Contains(l, "mobi") || strings.Contains(l, "iphone") || strings.Contains(l, "ipod"):
		device = "Mobile"
	case width > 0 && width < 600:
		device = "Mobile"
	case width > 0 && width < 1024 && strings.Contains(l, "macintosh") && strings.Contains(l, "safari") && !strings.Contains(l, "chrome"):
		device = "Tablet" // iPadOS asks for desktop sites with a Mac user agent
	default:
		device = "Desktop"
	}
	switch {
	case strings.Contains(l, "edg/") || strings.Contains(l, "edga/") || strings.Contains(l, "edgios/"):
		browser = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		browser = "Opera"
	case strings.Contains(l, "samsungbrowser"):
		browser = "Samsung Internet"
	case strings.Contains(l, "yabrowser"):
		browser = "Yandex Browser"
	case strings.Contains(l, "firefox/") || strings.Contains(l, "fxios/"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios/"):
		browser = "Chrome"
	case strings.Contains(l, "safari/"):
		browser = "Safari"
	default:
		browser = "Other"
	}
	switch {
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad") || strings.Contains(l, "ipod"):
		osName = "iOS"
	case strings.Contains(l, "android"):
		osName = "Android"
	case strings.Contains(l, "windows"):
		osName = "Windows"
	case strings.Contains(l, "cros"):
		osName = "ChromeOS"
	case strings.Contains(l, "mac os x") || strings.Contains(l, "macintosh"):
		osName = "macOS"
	case strings.Contains(l, "linux"):
		osName = "Linux"
	default:
		osName = "Other"
	}
	return
}

// analyticsIPExcluded reports whether ip matches one of the lines (addresses or CIDR ranges).
func analyticsIPExcluded(list, ip string) bool {
	if list == "" {
		return false
	}
	addr := net.ParseIP(ip)
	for _, l := range strings.Split(list, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.Contains(l, "/") {
			if _, n, err := net.ParseCIDR(l); err == nil && addr != nil && n.Contains(addr) {
				return true
			}
		} else if l == ip {
			return true
		}
	}
	return false
}

// analyticsLimit allows 300 requests a minute per address (a view sends two or three).
var analyticsLimit = &analyticsLimiter{hits: map[string]*wbFormHits{}}

type analyticsLimiter struct {
	mu    sync.Mutex
	hits  map[string]*wbFormHits
	swept time.Time
}

func (l *analyticsLimiter) allow(key string, now time.Time) bool {
	const window, limit = time.Minute, 300
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > window {
		for k, h := range l.hits {
			if now.Sub(h.start) >= window {
				delete(l.hits, k)
			}
		}
		l.swept = now
	}
	h := l.hits[key]
	if h == nil || now.Sub(h.start) >= window {
		h = &wbFormHits{start: now}
		l.hits[key] = h
	}
	h.n++
	return h.n <= limit
}
