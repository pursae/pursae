package server

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// Design tokens of the Pursae Web Builder: site-wide colours and fonts kept as CSS custom
// properties. One <style id="wb-tokens"> in the head of every page with builder parts defines
// them on :root, so a snapshot or a part's CSS that reads var(--wb-color-primary) follows a change
// at once, without publishing the pages again. Edited in Settings › Web Builder and through
// web-builder/v2/site-settings ("tokens").

const wbTokensOption = "web_builder_tokens"

// wbTokenColors / wbTokenFonts are the token names (CSS: --wb-color-<name>, --wb-font-<name>).
var (
	wbTokenColors = []string{"primary", "secondary", "accent", "text", "background"}
	wbTokenFonts  = []string{"heading", "body"}
)

type wbTokens struct {
	Colors map[string]string `json:"colors"`
	Fonts  map[string]string `json:"fonts"`
}

var (
	wbTokenHexRE   = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	wbTokenFuncRE  = regexp.MustCompile(`^(?:rgb|rgba|hsl|hsla)\(\s*[0-9.]+(?:deg|%)?(?:\s*[,\s/]\s*[0-9.]+%?){2,3}\s*\)$`)
	wbTokenFontRE  = regexp.MustCompile(`^[A-Za-z0-9 ,'"_-]{1,120}$`)
	wbTokenNamedRE = regexp.MustCompile(`^[a-zA-Z]{3,24}$`)
)

// wbTokenColor returns v when it is a safe CSS colour (hex, rgb/hsl function or a colour keyword).
func wbTokenColor(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if wbTokenHexRE.MatchString(v) || wbTokenFuncRE.MatchString(v) || wbTokenNamedRE.MatchString(v) {
		return v, true
	}
	return "", false
}

// wbTokenFont returns v when it is a plain font-family list (names, commas, quotes).
func wbTokenFont(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if wbTokenFontRE.MatchString(v) && strings.Count(v, `"`)%2 == 0 && strings.Count(v, `'`)%2 == 0 {
		return v, true
	}
	return "", false
}

// wbSanitizeTokens keeps only the known, valid tokens; invalid values are dropped.
func wbSanitizeTokens(in wbTokens) wbTokens {
	out := wbTokens{Colors: map[string]string{}, Fonts: map[string]string{}}
	for _, k := range wbTokenColors {
		if v, ok := wbTokenColor(in.Colors[k]); ok {
			out.Colors[k] = v
		}
	}
	for _, k := range wbTokenFonts {
		if v, ok := wbTokenFont(in.Fonts[k]); ok {
			out.Fonts[k] = v
		}
	}
	return out
}

// wbSiteTokens is the stored tokens (always sanitized, never nil maps).
func (s *Server) wbSiteTokens() wbTokens {
	var t wbTokens
	if raw := s.settings.String(wbTokensOption); raw != "" {
		_ = json.Unmarshal([]byte(raw), &t)
	}
	return wbSanitizeTokens(t)
}

// wbTokenCSS is the <style> element of the tokens, with the base rules of sub-menu levels (a
// menu-bound list indents wb-menu-d<level> links). Always printed on a page with builder parts: the
// menu rules are static, the :root block only appears once a token is set.
func wbTokenCSS(t wbTokens) string {
	var b strings.Builder
	b.WriteString("<style id=\"wb-tokens\">\n")
	var decl []string
	for _, k := range wbTokenColors {
		if v := t.Colors[k]; v != "" {
			decl = append(decl, "--wb-color-"+k+":"+v)
		}
	}
	for _, k := range wbTokenFonts {
		if v := t.Fonts[k]; v != "" {
			decl = append(decl, "--wb-font-"+k+":"+v)
		}
	}
	if len(decl) > 0 {
		b.WriteString(":root{" + strings.Join(decl, ";") + "}\n")
	}
	b.WriteString(".wb-menu-d1{padding-inline-start:1em}.wb-menu-d2{padding-inline-start:2em}\n")
	b.WriteString(wbDropdownCSS)
	b.WriteString("</style>\n")
	return b.String()
}

// wbSetTokens stores sanitized tokens (empty values clear a token).
func (s *Server) wbSetTokens(ctx context.Context, t wbTokens) error {
	raw, err := json.Marshal(wbSanitizeTokens(t))
	if err != nil {
		return err
	}
	return s.settings.Set(ctx, map[string]any{wbTokensOption: string(raw)})
}

// wbTokenField is one input of the design tokens section of Settings › Web Builder.
type wbTokenField struct {
	Name, Label, CSSVar, Value string
	Color                      bool
}

func (s *Server) wbTokenFields() []wbTokenField {
	t := s.wbSiteTokens()
	var out []wbTokenField
	for _, k := range wbTokenColors {
		out = append(out, wbTokenField{Name: "wb_token_color_" + k, Label: strings.ToUpper(k[:1]) + k[1:] + " color", CSSVar: "--wb-color-" + k, Value: t.Colors[k], Color: true})
	}
	for _, k := range wbTokenFonts {
		out = append(out, wbTokenField{Name: "wb_token_font_" + k, Label: strings.ToUpper(k[:1]) + k[1:] + " font", CSSVar: "--wb-font-" + k, Value: t.Fonts[k]})
	}
	return out
}

// wbDropdownCSS folds the sub-menus of a builder nav (data-wb-menu-tree="dropdown": a top-level
// item with sub-items is span.wb-menu-group holding its link and span.wb-menu-sub) into drop-downs
// that open on hover and keyboard focus, in the section's own colours (--bg, --fg, --line). In a
// mobile menu (details[data-wb-menu]) and a column menu (.wb-menu-col) they stay open below their
// parent, indented.
const wbDropdownCSS = `.wb-menu-group{position:relative;display:inline-flex;align-items:center}` +
	`.wb-menu-group>a.has-children::after{content:"";display:inline-block;width:.34em;height:.34em;margin-inline-start:.45em;border:solid currentColor;border-width:0 1.5px 1.5px 0;transform:translateY(-.18em) rotate(45deg);opacity:.7}` +
	`.wb-menu-sub{display:none;position:absolute;top:100%;inset-inline-start:0;z-index:60;min-width:12rem;flex-direction:column;padding:.4rem 0;background:var(--bg,#fff);color:var(--fg,#121418);border:1px solid var(--line,rgba(0,0,0,.1));border-radius:10px;box-shadow:0 18px 40px -18px rgba(0,0,0,.35)}` +
	`.wb-menu-group:hover>.wb-menu-sub,.wb-menu-group:focus-within>.wb-menu-sub{display:flex}` +
	`.wb-menu-sub>a{display:block;padding:.45em 1.1em;white-space:nowrap;color:inherit;text-align:start}` +
	`.wb-menu-sub>a.wb-menu-d2{padding-inline-start:2.1em}` +
	`details[data-wb-menu] .wb-menu-group,.wb-menu-col .wb-menu-group{display:block}` +
	`details[data-wb-menu] .wb-menu-sub,.wb-menu-col .wb-menu-sub{display:flex;position:static;min-width:0;padding:0 0 0 1em;background:none;border:0;border-radius:0;box-shadow:none}` +
	`details[data-wb-menu] .wb-menu-sub>a,.wb-menu-col .wb-menu-sub>a{padding:.35em 0}` +
	`details[data-wb-menu] .wb-menu-sub>a.wb-menu-d2,.wb-menu-col .wb-menu-sub>a.wb-menu-d2{padding-inline-start:1em}` + "\n"
