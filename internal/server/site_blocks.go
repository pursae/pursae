package server

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wpgo/internal/blocks"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// The remaining server-rendered core blocks of the block library: calendar, tag cloud, RSS, page
// list, login/out, social links, avatar and footnotes (render_block_core_* in wp-includes/blocks).

func registerSiteBlocks(s *Server) {
	blocks.RegisterDynamic("core/calendar", renderCalendarBlock)
	blocks.RegisterDynamic("core/tag-cloud", renderTagCloudBlock)
	blocks.RegisterDynamic("core/rss", renderRSSBlock)
	blocks.RegisterDynamic("core/page-list", renderPageListBlock)
	blocks.RegisterDynamic("core/loginout", renderLoginoutBlock)
	blocks.RegisterDynamic("core/social-links", renderSocialLinksBlock)
	blocks.RegisterDynamic("core/social-link", func(ctx context.Context, b *blocks.Block, _ string) string {
		return renderSocialLink(ctx, b, false, false)
	})
	blocks.RegisterDynamic("core/avatar", renderAvatarBlock)
	blocks.RegisterDynamic("core/footnotes", renderFootnotesBlock)
}

// Calendar -----------------------------------------------------------------------------------------

var weekdayInitials = []string{"S", "M", "T", "W", "T", "F", "S"}

func monthAbbrev(m int) string {
	if m < 1 || m > 12 {
		return ""
	}
	return time.Month(m).String()[:3]
}

// renderCalendarBlock is render_block_core_calendar() around get_calendar().
func renderCalendarBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	if ok, _ := rc.s.store.HasPublishedPosts(ctx, "post"); !ok {
		if rc.user != nil {
			return "<div>The calendar block is hidden because there are no published posts.</div>"
		}
		return ""
	}
	year, month := 0, 0
	if rc.q != nil && rc.q.Kind == "date" && rc.q.Month != 0 && rc.q.Year != 0 {
		year, month = rc.q.Year, rc.q.Month
	}
	// The block's own month/year only count with a %year%/%monthnum% permalink structure.
	if my, ok := b.Attrs["month"].(float64); ok {
		if yy, ok := b.Attrs["year"].(float64); ok {
			if st := rc.s.settings.String("permalink_structure"); strings.Contains(st, "%monthnum%") && strings.Contains(st, "%year%") {
				year, month = int(yy), int(my)
			}
		}
	}
	return fmt.Sprintf(`<div %s>%s</div>`, wrapperClass(b, ""), rc.s.getCalendar(ctx, rc, year, month))
}

// getCalendar is get_calendar( initial => true ): the month table with links to day archives and the
// previous/next month navigation.
func (s *Server) getCalendar(ctx context.Context, rc *renderCtx, year, month int) string {
	loc := s.loc()
	now := time.Now().In(loc)
	if year == 0 || month == 0 {
		year, month = now.Year(), int(now.Month())
	}
	weekBegins := s.settings.Int("start_of_week") % 7
	if weekBegins < 0 {
		weekBegins += 7
	}
	first := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc)
	daysInMonth := first.AddDate(0, 1, -1).Day()
	days, _ := s.store.PostDays(ctx, year, month, loc)
	withPost := map[int]bool{}
	for _, d := range days {
		withPost[d] = true
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "<table id=\"wp-calendar\" class=\"wp-calendar-table\">\n\t<caption>%s %d</caption>\n\t<thead>\n\t<tr>", time.Month(month).String(), year)
	for i := 0; i <= 6; i++ {
		wd := time.Weekday((i + weekBegins) % 7)
		fmt.Fprintf(&sb, "\n\t\t<th scope=\"col\" aria-label=\"%s\">%s</th>", wd.String(), weekdayInitials[int(wd)])
	}
	sb.WriteString("\n\t</tr>\n\t</thead>\n\t<tbody>\n\t<tr>")
	weekMod := func(n int) int { return ((n % 7) + 7) % 7 } // calendar_week_mod()
	if pad := weekMod(int(first.Weekday()) - weekBegins); pad != 0 {
		fmt.Fprintf(&sb, "\n\t\t<td colspan=\"%d\" class=\"pad\">&nbsp;</td>", pad)
	}
	newRow := false
	var last time.Time
	for day := 1; day <= daysInMonth; day++ {
		if newRow {
			sb.WriteString("\n\t</tr>\n\t<tr>\n\t\t")
		}
		newRow = false
		if now.Day() == day && int(now.Month()) == month && now.Year() == year {
			sb.WriteString(`<td id="today">`)
		} else {
			sb.WriteString("<td>")
		}
		d := time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)
		last = d
		if withPost[day] {
			label := "Posts published on " + d.Format("January 2, 2006")
			fmt.Fprintf(&sb, `<a href="%s" aria-label="%s">%d</a>`, html.EscapeString(rc.lb.DayLink(year, month, day)), html.EscapeString(label), day)
		} else {
			sb.WriteString(strconv.Itoa(day))
		}
		sb.WriteString("</td>")
		if weekMod(int(d.Weekday())-weekBegins) == 6 {
			newRow = true
		}
	}
	if pad := 7 - weekMod(int(last.Weekday())-weekBegins); pad != 0 && pad != 7 {
		fmt.Fprintf(&sb, "\n\t\t<td class=\"pad\" colspan=\"%d\">&nbsp;</td>", pad)
	}
	sb.WriteString("\n\t</tr>\n\t</tbody>\n\t</table>")

	sb.WriteString(`<nav aria-label="Previous and next months" class="wp-calendar-nav">`)
	if py, pm, ok, _ := s.store.AdjacentMonth(ctx, year, month, loc, true); ok {
		fmt.Fprintf(&sb, "\n\t\t<span class=\"wp-calendar-nav-prev\"><a href=\"%s\">&laquo; %s</a></span>", html.EscapeString(rc.lb.MonthLink(py, pm)), monthAbbrev(pm))
	} else {
		sb.WriteString("\n\t\t<span class=\"wp-calendar-nav-prev\">&nbsp;</span>")
	}
	sb.WriteString("\n\t\t<span class=\"pad\">&nbsp;</span>")
	if ny, nm, ok, _ := s.store.AdjacentMonth(ctx, year, month, loc, false); ok {
		fmt.Fprintf(&sb, "\n\t\t<span class=\"wp-calendar-nav-next\"><a href=\"%s\">%s &raquo;</a></span>", html.EscapeString(rc.lb.MonthLink(ny, nm)), monthAbbrev(nm))
	} else {
		sb.WriteString("\n\t\t<span class=\"wp-calendar-nav-next\">&nbsp;</span>")
	}
	sb.WriteString("\n\t</nav>")
	return sb.String()
}

// Tag cloud ----------------------------------------------------------------------------------------

var fontSizeUnitRe = regexp.MustCompile(`(?i)^[0-9.]+([a-z%]+)$`)

// phpFloat formats a float the way PHP casts it to a string (14 significant digits).
func phpFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', 14, 64)
}

// renderTagCloudBlock is render_block_core_tag_cloud() with wp_tag_cloud() / wp_generate_tag_cloud().
func renderTagCloudBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	smallestStr := attrStr(b, "smallestFontSize", "8pt")
	largestStr := attrStr(b, "largestFontSize", "22pt")
	unit := "pt"
	if m := fontSizeUnitRe.FindStringSubmatch(smallestStr); m != nil {
		unit = m[1]
	}
	parse := func(v string, def float64) float64 {
		n := strings.TrimRight(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ%")
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f
		}
		return def
	}
	smallest, largest := parse(smallestStr, 8), parse(largestStr, 22)
	number := attrInt(b, "numberOfTags", 45)
	taxonomy := attrStr(b, "taxonomy", "post_tag")
	showCount := attrBool(b, "showTagCounts", false)

	// Always query the top tags by count, then show them by name (wp_tag_cloud()).
	terms, _, err := rc.s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: taxonomy, HideEmpty: true, OrderBy: "count", Order: "desc", PerPage: number})
	if err != nil || len(terms) == 0 {
		return ""
	}
	type tagData struct {
		term *store.Term
		pos  int // position in the count-ordered list
	}
	tags := make([]tagData, len(terms))
	for i, t := range terms {
		tags[i] = tagData{t, i + 1}
	}
	sort.SliceStable(tags, func(i, j int) bool {
		return strings.ToLower(tags[i].term.Name) < strings.ToLower(tags[j].term.Name)
	})
	scale := func(n int) float64 { // default_topic_count_scale()
		return math.Round(math.Log10(float64(n)+1) * 100)
	}
	minCount, maxCount := scale(tags[0].term.Count), scale(tags[0].term.Count)
	for _, t := range tags {
		c := scale(t.term.Count)
		if c < minCount {
			minCount = c
		}
		if c > maxCount {
			maxCount = c
		}
	}
	spread := maxCount - minCount
	if spread <= 0 {
		spread = 1
	}
	fontSpread := largest - smallest
	if fontSpread < 0 {
		fontSpread = 1
	}
	fontStep := fontSpread / spread
	ariaLabel := showCount || fontSpread != 0

	var out []string
	for i, t := range tags {
		count := t.term.Count
		formatted := formatNumber(count) + " items"
		if count == 1 {
			formatted = "1 item"
		}
		size := smallest + (scale(count)-minCount)*fontStep
		class := fmt.Sprintf("tag-cloud-link tag-link-%d tag-link-position-%d", t.term.ID, i+1)
		aria := ""
		if ariaLabel {
			aria = fmt.Sprintf(` aria-label="%s (%s)"`, html.EscapeString(t.term.Name), html.EscapeString(formatted))
		}
		cnt := ""
		if showCount {
			cnt = fmt.Sprintf(`<span class="tag-link-count"> (%d)</span>`, count)
		}
		out = append(out, fmt.Sprintf(`<a href="%s" class="%s" style="font-size: %s;"%s>%s%s</a>`,
			html.EscapeString(rc.lb.TermLink(t.term)), class, phpFloat(size)+unit, aria, html.EscapeString(t.term.Name), cnt))
	}
	return fmt.Sprintf(`<p %s>%s</p>`, wrapperClass(b, ""), strings.Join(out, "\n"))
}

// RSS ----------------------------------------------------------------------------------------------

const feedCacheLifetime = 12 * time.Hour // feed_cache_transient_lifetime

// renderRSSBlock is render_block_core_rss() over fetch_feed().
func renderRSSBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	feedURL := strings.TrimSpace(attrStr(b, "feedURL", ""))
	trimmed := strings.TrimSuffix(feedURL, "/")
	if trimmed == strings.TrimSuffix(rc.lb.HomeURL("/"), "/") || trimmed == strings.TrimSuffix(rc.s.cfg.SiteURL, "/") {
		return `<div class="components-placeholder"><div class="notice notice-error">Adding an RSS feed to this site&#8217;s homepage is not supported, as it could lead to a loop that slows down your site. Try using another block, like the <strong>Latest Posts</strong> block, to list posts from the site.</div></div>`
	}
	feed, err := rc.s.fetchFeed(ctx, feedURL)
	if err != nil {
		return `<div class="components-placeholder"><div class="notice notice-error"><strong>RSS Error:</strong> ` + html.EscapeString(err.Error()) + `</div></div>`
	}
	if len(feed.Items) == 0 {
		return `<div class="components-placeholder"><div class="notice notice-error">An error has occurred, which probably means the feed is down. Try again later.</div></div>`
	}
	items := feed.Items
	if n := attrInt(b, "itemsToShow", 5); n > 0 && n < len(items) {
		items = items[:n]
	}
	showDate, showAuthor, showExcerpt := attrBool(b, "displayDate", false), attrBool(b, "displayAuthor", false), attrBool(b, "displayExcerpt", false)
	loc := rc.s.loc()
	var sb strings.Builder
	for _, it := range items {
		title := html.EscapeString(strings.TrimSpace(wp.StripTags(it.Title)))
		if title == "" {
			title = "(no title)"
		}
		if link := escURL(it.Link); link != "" {
			title = "<a href='" + link + "'>" + title + "</a>"
		}
		sb.WriteString("<li class='wp-block-rss__item'><div class='wp-block-rss__item-title'>" + title + "</div>")
		if showDate && !it.Date.IsZero() {
			t := it.Date.In(loc)
			fmt.Fprintf(&sb, `<time datetime="%s" class="wp-block-rss__item-publish-date">%s</time> `, t.Format(time.RFC3339), html.EscapeString(wp.FormatDate(t, rc.s.settings.String("date_format"))))
		}
		if showAuthor && it.Author != "" {
			fmt.Fprintf(&sb, `<span class="wp-block-rss__item-author">by %s</span>`, html.EscapeString(wp.StripTags(it.Author)))
		}
		if showExcerpt {
			// wp_trim_words() escapes the words; esc_attr()/esc_html() leave existing entities such as [&hellip;] alone.
			ex := wp.TrimWords(html.UnescapeString(it.Description), attrInt(b, "excerptLength", 55), " [&hellip;]")
			if strings.HasSuffix(ex, "[...]") {
				ex = strings.TrimSuffix(ex, "[...]") + "[&hellip;]"
			}
			fmt.Fprintf(&sb, `<div class="wp-block-rss__item-excerpt">%s</div>`, ex)
		}
		sb.WriteString("</li>")
	}
	var classes []string
	if attrStr(b, "blockLayout", "list") == "grid" {
		classes = append(classes, "is-grid", "columns-"+strconv.Itoa(attrInt(b, "columns", 2)))
	}
	if showDate {
		classes = append(classes, "has-dates")
	}
	if showAuthor {
		classes = append(classes, "has-authors")
	}
	if showExcerpt {
		classes = append(classes, "has-excerpts")
	}
	return fmt.Sprintf(`<ul %s>%s</ul>`, wrapperClass(b, strings.Join(classes, " ")), sb.String())
}

// escURL is esc_url() for the schemes the RSS block links: http, https and relative URLs.
func escURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "mailto" {
		return ""
	}
	return html.EscapeString(raw)
}

type cachedFeed struct {
	Body    string    `json:"body"`
	Fetched time.Time `json:"fetched"`
}

// fetchFeed is fetch_feed(): the feed is downloaded with wp_safe_remote_request() rules (public hosts only,
// 10 s timeout) and kept in the transients table for feed_cache_transient_lifetime.
func (s *Server) fetchFeed(ctx context.Context, feedURL string) (*wp.Feed, error) {
	u, err := url.Parse(feedURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("A valid URL was not provided.")
	}
	sum := md5.Sum([]byte(feedURL))
	key := "feed_" + hex.EncodeToString(sum[:])
	var cached cachedFeed
	if ok, _ := s.store.GetTransient(ctx, key, &cached); ok && cached.Body != "" {
		if f, err := wp.ParseFeed([]byte(cached.Body)); err == nil {
			return f, nil
		}
	}
	body, ctype, status, err := safeFetch(ctx, feedURL)
	if err != nil {
		return nil, fmt.Errorf("A feed could not be found at `%s`. %s", feedURL, err.Error())
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("A feed could not be found at `%s`; the status code is `%d` and content-type is `%s`", feedURL, status, ctype)
	}
	f, err := wp.ParseFeed(body)
	if err != nil {
		return nil, fmt.Errorf("A feed could not be found at `%s`. This does not appear to be a valid RSS or Atom feed.", feedURL)
	}
	_ = s.store.SetTransient(ctx, key, cachedFeed{Body: string(body), Fetched: time.Now()}, feedCacheLifetime)
	return f, nil
}

// publicIP is the host check of wp_http_validate_url(): loopback, private, link-local, multicast and
// unspecified addresses are refused so a feed URL cannot reach internal services.
func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		// Carrier-grade NAT (100.64.0.0/10) and the IPv4 "this network" and broadcast ranges.
		if ip4[0] == 100 && ip4[1]&0xc0 == 64 || ip4[0] == 0 || ip4.Equal(net.IPv4bcast) {
			return false
		}
	}
	return true
}

// allowPrivateFeedHosts is switched on by tests so a local httptest feed can be fetched.
var allowPrivateFeedHosts = false

// safeFetch downloads a URL, refusing private hosts at connect time (after DNS resolution) and on every redirect.
func safeFetch(ctx context.Context, rawURL string) (body []byte, contentType string, status int, err error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if !allowPrivateFeedHosts && !publicIP(net.ParseIP(host)) {
			return errors.New("the address points at a private network")
		}
		return nil
	}}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("unsupported redirect scheme")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("User-Agent", "WordPress Go/"+Version+"; feed-fetcher")
	req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/rdf+xml;q=0.9, application/xml;q=0.8, text/xml;q=0.8, text/html;q=0.7, */*;q=0.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", 0, err
	}
	return body, resp.Header.Get("Content-Type"), resp.StatusCode, nil
}

// Page list ----------------------------------------------------------------------------------------

type pageListItem struct {
	page     *store.Post
	active   bool
	children []*pageListItem
}

// renderPageListBlock is render_block_core_page_list(): every published page nested under its parent,
// sorted by menu order then title (get_pages()).
func renderPageListBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	pages, _, err := rc.s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"page"}, Statuses: []string{"publish"}, OrderBy: "menu_order", Order: "asc", PerPage: -1})
	if err != nil || len(pages) == 0 {
		return ""
	}
	sort.SliceStable(pages, func(i, j int) bool {
		if pages[i].MenuOrder != pages[j].MenuOrder {
			return pages[i].MenuOrder < pages[j].MenuOrder
		}
		return strings.ToLower(pages[i].Title) < strings.ToLower(pages[j].Title)
	})
	var queried int64
	if rc.q != nil && rc.q.Post != nil {
		queried = rc.q.Post.ID
	}
	items := map[int64]*pageListItem{}
	var top []*pageListItem
	withChildren := map[int64][]*pageListItem{}
	ancestors := map[int64]bool{}
	for _, p := range pages {
		it := &pageListItem{page: p, active: p.ID == queried}
		if it.active {
			if chain, err := rc.s.store.Ancestors(ctx, p); err == nil {
				for _, a := range chain {
					ancestors[a.ID] = true
				}
			}
		}
		items[p.ID] = it
		if p.ParentID != 0 {
			withChildren[p.ParentID] = append(withChildren[p.ParentID], it)
		} else {
			top = append(top, it)
		}
	}
	var nest func(list []*pageListItem) []*pageListItem
	nest = func(list []*pageListItem) []*pageListItem {
		for _, it := range list {
			if kids := withChildren[it.page.ID]; len(kids) > 0 {
				it.children = nest(kids)
			}
		}
		return list
	}
	nested := nest(top)
	if parent := int64(attrInt(b, "parentPageID", 0)); parent != 0 {
		kids, ok := withChildren[parent]
		if !ok {
			return ""
		}
		nested = nest(kids)
	}
	frontID := int64(rc.s.settings.Int("page_on_front"))
	var render func(list []*pageListItem, depth int) string
	render = func(list []*pageListItem, depth int) string {
		var sb strings.Builder
		for _, it := range list {
			class, aria := "", ""
			if it.active {
				class += " current-menu-item"
				aria = ` aria-current="page"`
			}
			if ancestors[it.page.ID] {
				class += " current-menu-ancestor"
			}
			if len(it.children) > 0 {
				class += " has-child"
			}
			if it.page.ID == frontID {
				class += " menu-item-home"
			}
			title := wp.KsesPost(it.page.Title)
			fmt.Fprintf(&sb, `<li class="wp-block-pages-list__item%s"><a class="wp-block-pages-list__item__link" href="%s"%s>%s</a>`,
				html.EscapeString(class), html.EscapeString(rc.lb.Permalink(it.page)), aria, title)
			if len(it.children) > 0 {
				sb.WriteString(`<ul class="wp-block-navigation__submenu-container">` + render(it.children, depth+1) + `</ul>`)
			}
			sb.WriteString("</li>")
		}
		return sb.String()
	}
	markup := render(nested, 1)
	if attrBool(b, "isNested", false) {
		return markup
	}
	return fmt.Sprintf(`<ul %s>%s</ul>`, wrapperClass(b, ""), markup)
}

// Login / logout -----------------------------------------------------------------------------------

// renderLoginoutBlock is render_block_core_loginout(): wp_loginout() or wp_login_form().
func renderLoginoutBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	current := rc.lb.HomeURL("/")
	if rc.reqURI != "" {
		current = rc.lb.HomeURL(rc.reqURI)
	}
	redirect := ""
	if attrBool(b, "redirectToCurrent", true) {
		redirect = current
	}
	esc := html.EscapeString
	classes, contents := "logged-out", ""
	if rc.user != nil {
		classes = "logged-in"
		logout := "/wp-login?action=logout"
		if redirect != "" {
			logout += "&redirect_to=" + url.QueryEscape(redirect)
		}
		logout += "&_wpnonce=" + rc.logoutNonce
		contents = `<a href="` + esc(logout) + `">Log out</a>`
	} else if attrBool(b, "displayLoginAsForm", false) {
		classes += " has-login-form"
		contents = `<form name="loginform" id="loginform" action="/wp-login" method="post">` +
			`<p class="login-username"><label for="user_login">Username or Email Address</label><input type="text" name="log" id="user_login" autocomplete="username" class="input" value="" size="20" /></p>` +
			`<p class="login-password"><label for="user_pass">Password</label><input type="password" name="pwd" id="user_pass" autocomplete="current-password" spellcheck="false" class="input" value="" size="20" /></p>` +
			`<p class="login-remember"><label><input name="rememberme" type="checkbox" id="rememberme" value="forever" /> Remember Me</label></p>` +
			`<p class="login-submit"><input type="submit" name="wp-submit" id="wp-submit" class="button button-primary" value="Log In" /><input type="hidden" name="redirect_to" value="` + esc(current) + `" /></p>` +
			`</form>`
	} else {
		login := "/wp-login"
		if redirect != "" {
			login += "?redirect_to=" + url.QueryEscape(redirect)
		}
		contents = `<a href="` + esc(login) + `">Log in</a>`
	}
	return `<div ` + wrapperClass(b, classes) + `>` + contents + `</div>`
}

// Social links -------------------------------------------------------------------------------------

// socialServiceNames is block_core_social_link_services(): the label used when the block has none.
var socialServiceNames = map[string]string{
	"amazon": "Amazon", "bandcamp": "Bandcamp", "behance": "Behance", "bluesky": "Bluesky", "chain": "Link", "codepen": "CodePen",
	"deviantart": "DeviantArt", "discord": "Discord", "dribbble": "Dribbble", "dropbox": "Dropbox", "etsy": "Etsy", "facebook": "Facebook",
	"feed": "RSS Feed", "fivehundredpx": "500px", "flickr": "Flickr", "foursquare": "Foursquare", "goodreads": "Goodreads", "google": "Google",
	"github": "GitHub", "gravatar": "Gravatar", "instagram": "Instagram", "lastfm": "Last.fm", "linkedin": "LinkedIn", "mail": "Mail",
	"mastodon": "Mastodon", "meetup": "Meetup", "medium": "Medium", "patreon": "Patreon", "pinterest": "Pinterest", "pocket": "Pocket",
	"reddit": "Reddit", "share": "Share Icon", "skype": "Skype", "snapchat": "Snapchat", "soundcloud": "SoundCloud", "spotify": "Spotify",
	"telegram": "Telegram", "threads": "Threads", "tiktok": "TikTok", "tumblr": "Tumblr", "twitch": "Twitch", "twitter": "Twitter",
	"vimeo": "Vimeo", "vk": "VK", "whatsapp": "WhatsApp", "wordpress": "WordPress", "x": "X", "yelp": "Yelp", "youtube": "YouTube",
}

// socialExtraPaths are icons the block library ships that Twenty Twenty does not.
var socialExtraPaths = map[string]string{
	"x": `<path d="M13.982 10.622 20.54 3h-1.554l-5.693 6.618L8.745 3H3.5l6.876 10.007L3.5 21h1.554l6.012-6.989 4.8 6.989h5.245l-7.13-10.378Zm-2.128 2.474-.697-.997-5.543-7.93H8l4.474 6.4.697.996 5.815 8.318h-2.387l-4.745-6.787Z"></path>`,
}

var svgInnerRe = regexp.MustCompile(`(?s)<svg[^>]*>(.*)</svg>`)

// socialLinkIcon is block_core_social_link_get_icon(): the service's 24px icon, the chain icon otherwise.
func socialLinkIcon(service string) string {
	inner, ok := socialExtraPaths[service]
	if !ok {
		name := service
		switch service {
		case "fivehundredpx":
			name = "500px"
		case "chain", "share":
			name = "link"
		}
		for _, ic := range twentyTwentySocialIcons {
			if ic.Name == name {
				if m := svgInnerRe.FindStringSubmatch(ic.SVG); m != nil {
					inner, ok = m[1], true
				}
				break
			}
		}
	}
	if !ok && service != "chain" {
		return socialLinkIcon("chain")
	}
	return `<svg width="24" height="24" viewBox="0 0 24 24" version="1.1" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false">` + inner + `</svg>`
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// renderSocialLink is render_block_core_social_link(); openInNewTab and showLabels are the parent
// social-links block's context.
func renderSocialLink(_ context.Context, b *blocks.Block, openInNewTab, showLabels bool) string {
	rawURL := strings.TrimSpace(attrStr(b, "url", ""))
	if rawURL == "" {
		return ""
	}
	service := attrStr(b, "service", "Icon")
	label := strings.TrimSpace(attrStr(b, "label", ""))
	if label == "" {
		if n, ok := socialServiceNames[service]; ok {
			label = n
		} else {
			label = socialServiceNames["share"]
		}
	}
	if emailRe.MatchString(rawURL) {
		rawURL = "mailto:" + rawURL
	}
	if u, err := url.Parse(rawURL); (err != nil || u.Scheme == "") && !strings.HasPrefix(rawURL, "//") {
		rawURL = "https://" + rawURL
	}
	rel := strings.TrimSpace(attrStr(b, "rel", ""))
	relAttr, target := "", ""
	if openInNewTab {
		relAttr = ` rel="` + html.EscapeString(strings.TrimSpace(rel+" noopener nofollow")) + `"`
		target = ` target="_blank"`
	} else if rel != "" {
		relAttr = ` rel="` + html.EscapeString(rel) + `"`
	}
	labelClass := "wp-block-social-link-label"
	if !showLabels {
		labelClass += " screen-reader-text"
	}
	return `<li ` + wrapperClass(b, "wp-social-link wp-social-link-"+service) + `><a href="` + html.EscapeString(rawURL) + `" class="wp-block-social-link-anchor"` + relAttr + target + `>` +
		socialLinkIcon(service) + `<span class="` + labelClass + `">` + html.EscapeString(label) + `</span></a></li>`
}

// renderSocialLinksBlock re-renders the inner social-link blocks with the parent's openInNewTab /
// showLabels context and keeps the flex layout classes of the saved <ul>.
func renderSocialLinksBlock(ctx context.Context, b *blocks.Block, _ string) string {
	openInNewTab, showLabels := attrBool(b, "openInNewTab", false), attrBool(b, "showLabels", false)
	var sb strings.Builder
	inner := 0
	for _, chunk := range b.InnerContent {
		if chunk != nil {
			sb.WriteString(*chunk)
			continue
		}
		if inner < len(b.InnerBlocks) {
			child := b.InnerBlocks[inner]
			if child.Name() == "core/social-link" {
				sb.WriteString(renderSocialLink(ctx, child, openInNewTab, showLabels))
			} else {
				sb.WriteString(blocks.RenderBlock(ctx, child))
			}
		}
		inner++
	}
	return blocks.AddClass(sb.String(), "is-layout-flex wp-block-social-links-is-layout-flex")
}

// Avatar -------------------------------------------------------------------------------------------

// renderAvatarBlock is render_block_core_avatar() for the post author (userId attribute, the rendering
// post's author or the author archive).
func renderAvatarBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	size := attrInt(b, "size", 96)
	var authorID int64
	if id, ok := b.Attrs["userId"].(float64); ok {
		authorID = int64(id)
	} else if rc.postID != 0 {
		if p, err := rc.s.store.GetPost(ctx, rc.postID); err == nil {
			authorID = p.AuthorID
		}
	} else if rc.q != nil && rc.q.Author != nil {
		authorID = rc.q.Author.ID
	}
	if authorID == 0 {
		return ""
	}
	u, err := rc.s.store.UserByID(ctx, authorID)
	if err != nil {
		return ""
	}
	alt := html.EscapeString(u.DisplayName + " Avatar")
	img := fmt.Sprintf(`<img alt='%s' src='%s' srcset='%s 2x' class='avatar avatar-%d photo wp-block-avatar__image' height='%d' width='%d' loading='lazy' decoding='async'/>`,
		alt, html.EscapeString(rc.s.avatarFor(ctx, u.Email, size)), html.EscapeString(rc.s.avatarFor(ctx, u.Email, size*2)), size, size, size)
	if attrBool(b, "isLink", false) {
		target := attrStr(b, "linkTarget", "_self")
		label := ""
		if target == "_blank" {
			label = `aria-label="` + html.EscapeString("("+u.DisplayName+" author archive, opens in a new tab)") + `"`
		}
		img = fmt.Sprintf(`<a href="%s" target="%s" %s class="wp-block-avatar__link">%s</a>`, html.EscapeString(rc.lb.AuthorLink(u.ID, u.Nicename)), html.EscapeString(target), label, img)
	}
	return `<div ` + wrapperClass(b, "") + `>` + img + `</div>`
}

// Footnotes ----------------------------------------------------------------------------------------

// renderFootnotesBlock is render_block_core_footnotes(): the post's "footnotes" meta as an ordered list.
func renderFootnotesBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil || rc.postID == 0 {
		return ""
	}
	p, err := rc.s.store.GetPost(ctx, rc.postID)
	if err != nil || p.Password != "" && rc.c != nil && rc.s.passwordRequired(rc.c, p) {
		return ""
	}
	var notes []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	switch v := p.Meta["footnotes"].(type) {
	case string:
		if v == "" {
			return ""
		}
		_ = json.Unmarshal([]byte(v), &notes)
	case []any:
		raw, _ := json.Marshal(v)
		_ = json.Unmarshal(raw, &notes)
	}
	if len(notes) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, n := range notes {
		fmt.Fprintf(&sb, `<li id="%s">%s <a href="#%s-link" aria-label="Jump to footnote reference %d">↩︎</a></li>`, html.EscapeString(n.ID), n.Content, html.EscapeString(n.ID), i+1)
	}
	return `<ol ` + wrapperClass(b, "") + `>` + sb.String() + `</ol>`
}
