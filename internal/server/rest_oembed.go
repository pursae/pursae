package server

import (
	"encoding/xml"
	"fmt"
	"html"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/links"
	"wpgo/internal/store"
)

// WordPress as an oEmbed provider (wp-includes/class-wp-oembed-controller.php, embed.php): the
// /oembed/1.0/embed endpoint answers for this site's posts and pages, and /<post>/embed/ renders the
// embed template that other sites show in the iframe. The consumer side (/oembed/1.0/proxy) lives in
// rest_extra.go.

const (
	oembedDefaultWidth = 600 // get_oembed_response_data(): maxwidth is clamped to 200–600
	oembedMinWidth     = 200
	oembedMaxWidth     = 600
)

func (api *restAPI) registerOEmbed() {
	api.register("oembed/1.0", "/oembed/1.0/embed", map[string]restHandler{http.MethodGet: api.oembedEmbed}, nil)
}

// oembedResponse is get_oembed_response_data() in WordPress' field order.
type oembedResponse struct {
	Version      string `json:"version" xml:"version"`
	ProviderName string `json:"provider_name" xml:"provider_name"`
	ProviderURL  string `json:"provider_url" xml:"provider_url"`
	AuthorName   string `json:"author_name" xml:"author_name"`
	AuthorURL    string `json:"author_url" xml:"author_url"`
	Title        string `json:"title" xml:"title"`
	Type         string `json:"type" xml:"type"`
	Width        int    `json:"width" xml:"width"`
	Height       int    `json:"height" xml:"height"`
	HTML         string `json:"html" xml:"html"`
}

// oembedPost resolves one of this site's URLs to a publicly readable post or page (url_to_postid()).
func (s *Server) oembedPost(c *gin.Context, raw string) *store.Post {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return nil
	}
	home, err := url.Parse(s.links(c).Home)
	if err != nil || !strings.EqualFold(target.Hostname(), home.Hostname()) {
		return nil
	}
	path := strings.Trim(strings.TrimPrefix(target.Path, home.Path), "/")
	// Visibility is the visitor's: a logged-in editor's cookie must not make private posts embeddable.
	cc := c.Copy()
	cc.Request = c.Request.Clone(c.Request.Context())
	cc.Request.URL = target
	delete(cc.Keys, ctxUser)
	q, err := s.resolve(cc, path)
	if err != nil || q == nil || q.Post == nil || q.Post.Status != "publish" {
		return nil
	}
	switch q.Kind {
	case "single", "page", "front-page":
		return q.Post
	}
	return nil
}

// embedURL is get_post_embed_url().
func (s *Server) embedURL(c *gin.Context, p *store.Post) string {
	lb := s.links(c)
	link := lb.Permalink(p)
	if !lb.Pretty() {
		return link + previewSep(link) + "embed=true"
	}
	return strings.TrimRight(link, "/") + "/embed/"
}

// embedHTML is get_post_embed_html(): the blockquote fallback, the sandboxed iframe and wp-embed.js.
func (s *Server) embedHTML(c *gin.Context, p *store.Post, width, height int) string {
	secret := strings.ToLower(auth.RandomToken(10))
	if len(secret) > 10 {
		secret = secret[:10]
	}
	title := theTitle(p)
	out := fmt.Sprintf(`<blockquote class="wp-embedded-content" data-secret="%s"><a href="%s">%s</a></blockquote>`,
		secret, html.EscapeString(s.links(c).Permalink(p)), title)
	out += fmt.Sprintf(`<iframe sandbox="allow-scripts" security="restricted" style="position: absolute; visibility: hidden;" src="%s#?secret=%s" data-secret="%s" width="%d" height="%d" title="%s" frameborder="0" marginwidth="0" marginheight="0" scrolling="no" class="wp-embedded-content"></iframe>`,
		html.EscapeString(s.embedURL(c, p)), secret, secret, width, height, "&#8220;"+title+"&#8221; &#8212; "+html.EscapeString(s.settings.String("blogname")))
	if js := s.webFile("wp-includes/js/wp-embed.min.js"); js != "" {
		out += `<script type="text/javascript">` + strings.ReplaceAll(js, "</script>", "") + `</script>`
	}
	return out
}

// oembedEmbed is WP_oEmbed_Controller::get_item().
func (api *restAPI) oembedEmbed(c *gin.Context, r *restRequest) {
	s := api.s
	raw, ok := c.GetQuery("url")
	if !ok || raw == "" {
		restErr(c, http.StatusBadRequest, "rest_missing_callback_param", "Missing parameter(s): url", map[string]any{"params": []string{"url"}})
		return
	}
	format := c.DefaultQuery("format", "json")
	if format != "json" && format != "xml" {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): format", map[string]any{"params": map[string]string{"format": "format is not one of json, and xml."}})
		return
	}
	width := oembedDefaultWidth
	if v := c.Query("maxwidth"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): maxwidth", map[string]any{"params": map[string]string{"maxwidth": "maxwidth is not of type integer."}})
			return
		}
		width = n
	}
	width = max(oembedMinWidth, min(oembedMaxWidth, width))
	p := s.oembedPost(c, raw)
	if p == nil {
		restErr(c, http.StatusNotFound, "oembed_invalid_url", "Not Found")
		return
	}
	height := max(int(math.Ceil(float64(width)/16*9)), 200)
	lb := s.links(c)
	data := oembedResponse{Version: "1.0", ProviderName: s.settings.String("blogname"), ProviderURL: lb.Home, Title: theTitle(p),
		Type: "rich", Width: width, Height: height, HTML: s.embedHTML(c, p, width, height)}
	if u, err := s.store.UserByID(c, p.AuthorID); err == nil {
		data.AuthorName, data.AuthorURL = u.DisplayName, lb.AuthorLink(u.ID, u.Nicename)
	}
	if format == "xml" {
		// _oembed_rest_pre_serve_request() / _oembed_create_xml()
		var sb strings.Builder
		sb.WriteString(`<?xml version="1.0" encoding="utf-8" standalone="yes"?><oembed>`)
		for _, kv := range [][2]string{{"version", data.Version}, {"provider_name", data.ProviderName}, {"provider_url", data.ProviderURL},
			{"author_name", data.AuthorName}, {"author_url", data.AuthorURL}, {"title", data.Title}, {"type", data.Type},
			{"width", strconv.Itoa(data.Width)}, {"height", strconv.Itoa(data.Height)}, {"html", data.HTML}} {
			sb.WriteString("<" + kv[0] + ">")
			_ = xml.EscapeText(&sb, []byte(kv[1]))
			sb.WriteString("</" + kv[0] + ">")
		}
		sb.WriteString("</oembed>")
		c.Data(http.StatusOK, "text/xml; charset=utf-8", []byte(sb.String()))
		return
	}
	c.JSON(http.StatusOK, data)
}

// embedRoute serves /<post>/embed/ and ?embed=true (the embed template, embed.php); other requests pass on to the site.
func (s *Server) embedRoute(c *gin.Context) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.Next()
		return
	}
	path := strings.Trim(c.Request.URL.Path, "/")
	switch {
	case path == "embed" || strings.HasSuffix(path, "/embed"):
		path = strings.TrimSuffix(strings.TrimSuffix(path, "embed"), "/")
	case c.Query("embed") == "true":
	default:
		c.Next()
		return
	}
	if strings.HasPrefix(path, "wp-") {
		c.Next()
		return
	}
	c.Header("X-WP-embed", "true")
	c.Header("Content-Type", "text/html; charset=UTF-8")
	q, err := s.resolve(c, path)
	var p *store.Post
	if err == nil && q != nil && q.Post != nil && (q.Kind == "single" || q.Kind == "page" || q.Kind == "front-page") {
		p = q.Post
	}
	if p == nil {
		c.String(http.StatusNotFound, s.embedPage(c, nil))
		c.Abort()
		return
	}
	c.String(http.StatusOK, s.embedPage(c, p))
	c.Abort()
}

// embedPage renders wp-includes/theme-compat/embed.php with embed-content.php (or embed-404.php).
func (s *Server) embedPage(c *gin.Context, p *store.Post) string {
	lb := s.links(c)
	site := html.EscapeString(s.settings.String("blogname"))
	home := html.EscapeString(lb.HomeURL("/"))
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>` + "\n" + `<html lang="en-US" class="no-js">` + "\n<head>\n")
	if p != nil {
		sb.WriteString("\t<title>" + theTitle(p) + " &#8211; " + site + "</title>\n")
	} else {
		sb.WriteString("\t<title>Page not found &#8211; " + site + "</title>\n")
	}
	sb.WriteString("\t<meta http-equiv=\"X-UA-Compatible\" content=\"IE=edge\">\n\t<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	sb.WriteString("\t<meta name=\"robots\" content=\"noindex, follow\" />\n")
	if p != nil {
		sb.WriteString("\t<link rel=\"canonical\" href=\"" + html.EscapeString(lb.Permalink(p)) + "\" />\n")
	}
	sb.WriteString("\t<style type=\"text/css\">\n" + s.webFile("wp-includes/css/wp-embed-template.min.css") + "\n\t</style>\n")
	sb.WriteString("</head>\n<body class=\"wp-embed-responsive\">\n")
	if p == nil {
		// embed-404.php
		sb.WriteString("<div class=\"wp-embed\">\n\t<p class=\"wp-embed-heading\">Oops! That embed cannot be found.</p>\n\n")
		sb.WriteString("\t<div class=\"wp-embed-excerpt\"><p>It looks like nothing was found at this location. Maybe try visiting <a href=\"" + home + "\">" + site + "</a> directly?</p></div>\n\n")
		s.embedFooter(&sb, c, nil, site, home)
		sb.WriteString("</div>\n")
	} else {
		// embed-content.php
		link := html.EscapeString(lb.Permalink(p))
		classes := []string{"wp-embed", "post-" + itoa64(p.ID), p.Type, "type-" + p.Type, "status-" + p.Status, "hentry"}
		if p.Type == "post" {
			classes = append(classes, "format-"+defaultStr(p.Format, "standard"))
		}
		sb.WriteString("<div class=\"" + strings.Join(classes, " ") + "\">\n")
		if p.FeaturedMediaID > 0 {
			if a, err := s.store.GetAttachment(c, p.FeaturedMediaID); err == nil && a.Width > 0 {
				shape, size := "rectangular", "large"
				if float64(a.Width)/float64(max(a.Height, 1)) < 1.75 {
					shape, size = "square", "full"
				}
				src, w, h := s.uploadURL(c, a.FilePath), a.Width, a.Height
				if v := a.Variant(size); v != nil {
					src, w, h = s.uploadURL(c, v.FilePath), v.Width, v.Height
				}
				fmt.Fprintf(&sb, "\t<div class=\"wp-embed-featured-image %s\">\n\t\t<a href=\"%s\" target=\"_top\">\n\t\t\t<img width=\"%d\" height=\"%d\" src=\"%s\" class=\"attachment-%s size-%s wp-post-image\" alt=\"%s\" decoding=\"async\" />\t\t</a>\n\t</div>\n",
					shape, link, w, h, html.EscapeString(src), size, size, html.EscapeString(a.Alt))
			}
		}
		sb.WriteString("\t<p class=\"wp-embed-heading\">\n\t\t<a href=\"" + link + "\" target=\"_top\">\n\t\t\t" + theTitle(p) + "\t\t</a>\n\t</p>\n\n")
		excerpt := s.theExcerpt(p, lb)
		if p.Password != "" {
			// the_excerpt_embed() shows the password form instead of the content.
			excerpt = "<p>This content is password protected. To view it please enter your password below:</p>\n" +
				"<form action=\"" + html.EscapeString(lb.HomeURL("/wp-login?action=postpass")) + "\" class=\"post-password-form\" method=\"post\" target=\"_top\"><p><label for=\"pwbox-" + itoa64(p.ID) + "\">Password: <input name=\"post_password\" id=\"pwbox-" + itoa64(p.ID) + "\" type=\"password\" spellcheck=\"false\" size=\"20\" /></label> <input type=\"submit\" name=\"Submit\" value=\"Enter\" /></p></form>\n"
		}
		sb.WriteString("\t<div class=\"wp-embed-excerpt\">" + excerpt + "</div>\n\n")
		s.embedFooter(&sb, c, p, site, home)
		sb.WriteString("</div>\n")
	}
	if p != nil {
		// print_embed_sharing_dialog()
		fmt.Fprintf(&sb, "<div class=\"wp-embed-share-dialog hidden\" role=\"dialog\" aria-label=\"Sharing options\">\n\t<div class=\"wp-embed-share-dialog-content\">\n\t\t<div class=\"wp-embed-share-dialog-text\">\n\t\t\t<ul class=\"wp-embed-share-tabs\" role=\"tablist\">\n\t\t\t\t<li class=\"wp-embed-share-tab-button wp-embed-share-tab-button-wordpress\" role=\"presentation\">\n\t\t\t\t\t<button type=\"button\" role=\"tab\" aria-controls=\"wp-embed-share-tab-wordpress\" aria-selected=\"true\" tabindex=\"0\">WordPress Embed</button>\n\t\t\t\t</li>\n\t\t\t\t<li class=\"wp-embed-share-tab-button wp-embed-share-tab-button-html\" role=\"presentation\">\n\t\t\t\t\t<button type=\"button\" role=\"tab\" aria-controls=\"wp-embed-share-tab-html\" aria-selected=\"false\" tabindex=\"-1\">HTML Embed</button>\n\t\t\t\t</li>\n\t\t\t</ul>\n\t\t\t<div id=\"wp-embed-share-tab-wordpress\" class=\"wp-embed-share-tab\" role=\"tabpanel\" aria-hidden=\"false\">\n\t\t\t\t<input type=\"text\" value=\"%s\" class=\"wp-embed-share-input\" aria-describedby=\"wp-embed-share-description-wordpress\" tabindex=\"0\" readonly/>\n\n\t\t\t\t<p class=\"wp-embed-share-description\" id=\"wp-embed-share-description-wordpress\">Copy and paste this URL into your WordPress site to embed</p>\n\t\t\t</div>\n\t\t\t<div id=\"wp-embed-share-tab-html\" class=\"wp-embed-share-tab\" role=\"tabpanel\" aria-hidden=\"true\">\n\t\t\t\t<textarea class=\"wp-embed-share-input\" aria-describedby=\"wp-embed-share-description-html\" tabindex=\"0\" readonly>%s</textarea>\n\n\t\t\t\t<p class=\"wp-embed-share-description\" id=\"wp-embed-share-description-html\">Copy and paste this code into your site to embed</p>\n\t\t\t</div>\n\t\t</div>\n\n\t\t<button type=\"button\" class=\"wp-embed-share-dialog-close\" aria-label=\"Close sharing dialog\">\n\t\t\t<span class=\"dashicons dashicons-no\"></span>\n\t\t</button>\n\t</div>\n</div>\n",
			html.EscapeString(lb.Permalink(p)), html.EscapeString(s.embedHTML(c, p, oembedDefaultWidth, 338)))
	}
	if js := s.webFile("wp-includes/js/wp-embed-template.min.js"); js != "" {
		sb.WriteString("<script type=\"text/javascript\">\n" + strings.ReplaceAll(js, "</script>", "") + "\n</script>\n")
	}
	sb.WriteString("</body>\n</html>\n")
	return sb.String()
}

// embedFooter is the_embed_site_title(), print_embed_comments_button() and print_embed_sharing_button().
func (s *Server) embedFooter(sb *strings.Builder, c *gin.Context, p *store.Post, site, home string) {
	sb.WriteString("\t<div class=\"wp-embed-footer\">\n")
	icon := ""
	if id := int64(s.settings.Int("site_icon")); id > 0 {
		if a, err := s.store.GetAttachment(c, id); err == nil {
			src := s.uploadURL(c, a.FilePath)
			if v := a.Variant("thumbnail"); v != nil {
				src = s.uploadURL(c, v.FilePath)
			}
			icon = "<img src=\"" + html.EscapeString(src) + "\" srcset=\"" + html.EscapeString(src) + " 2x\" width=\"32\" height=\"32\" alt=\"\" class=\"wp-embed-site-icon\" />\n\t\t\t\t"
		}
	}
	sb.WriteString("\t\t<div class=\"wp-embed-site-title\">\n\t\t\t<a href=\"" + home + "\" target=\"_top\">\n\t\t\t\t" + icon + "<span>" + site + "</span>\n\t\t\t</a>\n\t\t</div>\n")
	sb.WriteString("\t\t<div class=\"wp-embed-meta\">\n")
	if p != nil && (p.CommentCount > 0 || p.CommentStatus == "open") {
		label := strconv.Itoa(p.CommentCount) + " Comments"
		if p.CommentCount == 1 {
			label = "1 Comment"
		}
		sb.WriteString("\t\t\t<div class=\"wp-embed-comments\">\n\t\t\t\t<a href=\"" + html.EscapeString(s.links(c).Permalink(p)) + "#comments\" target=\"_top\">\n\t\t\t\t\t<span class=\"dashicons dashicons-admin-comments\"></span>\n\t\t\t\t\t" + label + "\t\t\t\t</a>\n\t\t\t</div>\n")
	}
	if p != nil {
		sb.WriteString("\t\t\t<div class=\"wp-embed-share\">\n\t\t\t\t<button type=\"button\" class=\"wp-embed-share-dialog-open\" aria-label=\"Open sharing dialog\">\n\t\t\t\t\t<span class=\"dashicons dashicons-share\"></span>\n\t\t\t\t</button>\n\t\t\t</div>\n")
	}
	sb.WriteString("\t\t</div>\n\t</div>\n")
}

// oembedDiscoveryLinks is wp_oembed_add_discovery_links(): the <link rel="alternate"> tags on singular views.
func oembedDiscoveryLinks(lb *links.Builder, q *siteQuery) string {
	if (q.Kind != "single" && q.Kind != "page" && q.Kind != "front-page") || q.Post == nil {
		return ""
	}
	endpoint := html.EscapeString(lb.HomeURL("/wp-json/oembed/1.0/embed?url=" + url.QueryEscape(lb.Permalink(q.Post))))
	return "<link rel=\"alternate\" title=\"oEmbed (JSON)\" type=\"application/json+oembed\" href=\"" + endpoint + "\" />\n" +
		"<link rel=\"alternate\" title=\"oEmbed (XML)\" type=\"text/xml+oembed\" href=\"" + endpoint + "&#038;format=xml\" />\n"
}
