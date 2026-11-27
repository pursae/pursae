package server

import (
	"bytes"
	"context"
	"html"
	"strings"

	"github.com/gin-gonic/gin"
	xhtml "golang.org/x/net/html"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Scripts an administrator stored.
//
// WordPress lets a user with unfiltered_html put a <script> in a post, a Custom HTML block or
// widget, a Web Builder html section, the maintenance page, …; every save path runs everybody
// else's markup through KSES, which drops scripts. So a script in such stored markup was written
// by a trusted user, and a page that shows it gives it the nonce: cspTrustScripts adds the nonce
// placeholder to the <script> start tags of that markup (and of the documents in its srcdoc
// iframes, which inherit the page's policy).
//
// It is called on the stored value, where it is read for the page, never on finished output: the
// shortcodes, plugin filters and request data that later surround it stay without a nonce, so a
// script that a bug reflects into the page is still blocked. Only an HTML page that the outermost
// handler swaps gets the placeholder (renderCtx.page); feeds, REST, e-mails and other renders of
// the same content are left as stored.
//
// The csp_admin_scripts option ("1" by default) turns this off: under the enforced policy stored
// scripts are then blocked too.

// cspTrustOn reports whether stored administrator scripts get the nonce on this site.
func (s *Server) cspTrustOn() bool {
	return s.settings == nil || s.settings.Bool("csp_admin_scripts")
}

// cspTrustPage reports whether ctx renders an HTML page whose stored administrator scripts get the
// nonce.
func (s *Server) cspTrustPage(ctx context.Context) bool {
	rc := renderFrom(ctx)
	return rc != nil && rc.page && !rc.feed && s.cspTrustOn()
}

// cspTrustTypes are the post types whose content every save path filters (kses) for users without
// unfiltered_html. A Query Loop can show any post type, and some keep raw markup that a lesser
// capability saves (Contact Form templates need only publish_pages), so the Post Content block
// trusts these only.
var cspTrustTypes = map[string]bool{"post": true, "page": true, "product": true, "wp_block": true}

// cspTrustPostContent is cspTrustContent for the content of p when p's type is in cspTrustTypes;
// other types render as stored.
func (s *Server) cspTrustPostContent(ctx context.Context, p *store.Post) string {
	if !cspTrustTypes[p.Type] {
		return p.Content
	}
	return s.cspTrustContent(ctx, p.Content)
}

// cspTrustStored is cspTrustScripts for stored markup that a page outside the theme's render (the
// maintenance page) shows, when the site trusts stored administrator scripts.
func (s *Server) cspTrustStored(markup string) string {
	if !s.cspTrustOn() {
		return markup
	}
	return cspTrustScripts(markup)
}

// cspTrustContent is cspTrustScripts for stored markup rendered by ctx (see cspTrustPage).
func (s *Server) cspTrustContent(ctx context.Context, markup string) string {
	if !s.cspTrustPage(ctx) {
		return markup
	}
	return cspTrustScripts(markup)
}

// cspTrustScripts gives every <script> start tag in markup the nonce placeholder, replacing a nonce
// attribute the markup carries, and does the same inside srcdoc attributes. Only a script closed by
// its </script> in markup gets it: an unclosed one (a fragment cut at <!--more-->, broken markup)
// would run whatever follows in the page as its code. Markup without a script is returned as it is.
func cspTrustScripts(markup string) string {
	if !containsFold(markup, "<script") && !containsFold(markup, "srcdoc") {
		return markup
	}
	var b strings.Builder
	b.Grow(len(markup) + 64)
	var raw, nonced []byte // an open script: its start tag as written and with the nonce
	open := false
	z := xhtml.NewTokenizer(strings.NewReader(markup))
	for {
		tt := z.Next()
		tok := z.Raw()
		if tt == xhtml.ErrorToken {
			if open { // never closed: as written
				b.Write(raw)
			}
			b.Write(tok) // whatever the tokenizer could not finish (it is not a tag)
			return b.String()
		}
		if open {
			if tt == xhtml.EndTagToken {
				if name, _ := z.TagName(); bytes.Equal(name, []byte("script")) {
					b.Write(nonced)
					b.Write(tok)
					open = false
					continue
				}
			}
			raw, nonced = append(raw, tok...), append(nonced, tok...)
			continue
		}
		if tt != xhtml.StartTagToken && tt != xhtml.SelfClosingTagToken {
			b.Write(tok)
			continue
		}
		name, hasAttr := z.TagName()
		switch {
		case bytes.Equal(name, []byte("script")):
			raw = append([]byte(nil), tok...)
			nonced = []byte(rebuildTag("script", z, hasAttr, tt, func(key, val string) (string, bool) {
				return val, key != "nonce"
			}, cspNonceAttr()))
			open = true
		case hasAttr && bytes.Equal(name, []byte("iframe")) && containsFold(string(tok), "srcdoc"):
			b.WriteString(rebuildTag("iframe", z, hasAttr, tt, func(key, val string) (string, bool) {
				if key == "srcdoc" {
					return cspTrustScripts(val), true
				}
				return val, true
			}, ""))
		default:
			b.Write(tok)
		}
	}
}

// rebuildTag writes the current start tag of z again, each attribute passed through keep (which
// may change the value or drop it), then extra.
func rebuildTag(name string, z *xhtml.Tokenizer, hasAttr bool, tt xhtml.TokenType, keep func(key, val string) (string, bool), extra string) string {
	var b strings.Builder
	b.WriteString("<" + name)
	for more := hasAttr; more; {
		var k, v []byte
		k, v, more = z.TagAttr()
		val, ok := keep(string(k), string(v))
		if !ok {
			continue
		}
		b.WriteString(" " + string(k) + `="` + html.EscapeString(val) + `"`)
	}
	b.WriteString(extra)
	if tt == xhtml.SelfClosingTagToken {
		b.WriteString(" /")
	}
	b.WriteString(">")
	return b.String()
}

// containsFold reports whether s contains the ASCII substring sub, ignoring case.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), sub)
}

// The settings API (/pursae/v1/options/…) needs only manage_options, which a settings-scoped
// application password has without unfiltered_html. These checks keep its raw markup fields to
// the same rule as their screens: changing raw code needs unfiltered_html.

const errNeedsUnfiltered = "Changing raw HTML or script code needs the unfiltered_html capability."

// optionsActor is the user a settings check judges: the REST caller (an Application Password or OAuth
// token, already narrowed to its scopes) when the request came through the API, else the signed-in user.
// currentUser alone is only the cookie session, so a token request would be refused even with full access.
func optionsActor(c *gin.Context) *store.User {
	if u := restCurrent(c); u != nil {
		return u
	}
	return currentUser(c)
}

// maintCheckRaw: the custom HTML only with unfiltered_html; the content and footer filtered (kses)
// otherwise, as on the Maintenance screen.
func (s *Server) maintCheckRaw(c *gin.Context, old, st maintSettings) []string {
	if optionsActor(c).Can("unfiltered_html", nil) {
		return nil
	}
	raw := func(cur, prev string) bool { return cur != prev && wp.KsesPost(cur) != cur }
	// Custom CSS is raw code like the custom HTML (the WordPress rule for Additional CSS).
	if st.CustomHTML != old.CustomHTML || st.CustomCSS != old.CustomCSS || raw(st.Content, old.Content) || raw(st.FooterText, old.FooterText) {
		return []string{errNeedsUnfiltered}
	}
	return nil
}

// seogoCheckRaw: the RSS texts are HTML in every feed item; markup KSES would change needs
// unfiltered_html, as the SEO screen filters it otherwise.
func (s *Server) seogoCheckRaw(c *gin.Context, old, st *seogoOptions) []string {
	if optionsActor(c).Can("unfiltered_html", nil) {
		return nil
	}
	raw := func(cur, prev string) bool { return cur != prev && wp.KsesPost(cur) != cur }
	if raw(st.RSSBefore, old.RSSBefore) || raw(st.RSSAfter, old.RSSAfter) {
		return []string{errNeedsUnfiltered}
	}
	return nil
}

// gooikeCheckRaw: the code of custom (non-service) scripts only with unfiltered_html.
func (s *Server) gooikeCheckRaw(c *gin.Context, old, st gooikeSettings) []string {
	if optionsActor(c).Can("unfiltered_html", nil) {
		return nil
	}
	prev := map[string]string{}
	for _, sc := range old.Scripts {
		if gooikeServiceByKey(sc.Service) == nil {
			prev[sc.ID] = sc.Code
		}
	}
	for _, sc := range st.Scripts {
		if code, ok := prev[sc.ID]; gooikeServiceByKey(sc.Service) == nil && (!ok || code != sc.Code) {
			return []string{errNeedsUnfiltered}
		}
	}
	return nil
}
