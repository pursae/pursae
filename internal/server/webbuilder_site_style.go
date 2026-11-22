package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"wpgo/internal/webbuilder"
)

// The site style of the Pursae Web Builder (the Site Editor's global styles): one brand colour,
// font pair, heading and body fonts and corner style for the whole site. A design (page or
// template) whose site settings carry "global": true follows it: the editor starts new designs
// with it, the server puts the current style into such a design whenever it is loaded
// (wbDocumentFor), and after the style changes the designs that follow it are listed as outdated,
// so Settings › Web Builder › Regenerate pages publishes them again with the new look (snapshots
// are static HTML drawn by the editor). Saved through web-builder/v2/site-settings ("site_style").

const wbSiteStyleOption = "web_builder_site_style"

// wbSiteStyleKeys are the doc.site keys the site style sets ("global" is the design's own flag).
var wbSiteStyleKeys = []string{"brand", "fontPair", "corners", "headingFont", "headingWeight", "bodyFont", "bodyWeight"}

// wbSiteStyle is the stored site style, or nil while none was saved (or it no longer validates).
func (s *Server) wbSiteStyle() *webbuilder.SiteSettings {
	raw := s.settings.String(wbSiteStyleOption)
	if raw == "" {
		return nil
	}
	st, err := wbParseSiteStyle([]byte(raw))
	if err != nil {
		return nil
	}
	return st
}

// wbParseSiteStyle validates a site style with the doc validator's rules for doc.site; brand,
// fontPair and corners are required.
func wbParseSiteStyle(raw []byte) (*webbuilder.SiteSettings, error) {
	var st webbuilder.SiteSettings
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("site_style must be an object of brand, fontPair, corners and fonts")
	}
	st.Global = false
	if st.Brand == "" || st.FontPair == "" || st.Corners == "" {
		return nil, fmt.Errorf("site_style needs brand, fontPair and corners")
	}
	doc, _ := json.Marshal(map[string]any{"sections": []any{}, "site": st})
	r, err := webbuilder.ParseDoc(doc, webbuilder.DocOptions{})
	if err != nil {
		return nil, fmt.Errorf("site_style is not valid")
	}
	if !r.Valid() {
		return nil, fmt.Errorf("site_style is not valid: %s", r.Errors[0].Path)
	}
	return &st, nil
}

// wbSetSiteStyle stores a validated site style (nil removes it).
func (s *Server) wbSetSiteStyle(ctx context.Context, st *webbuilder.SiteSettings) error {
	val := ""
	if st != nil {
		b, err := json.Marshal(st)
		if err != nil {
			return err
		}
		val = string(b)
	}
	return s.settings.Set(ctx, map[string]any{wbSiteStyleOption: val})
}

// wbDocFollowStyle puts the site style into a stored doc that follows it (site.global true);
// any other doc, or no style, returns raw unchanged.
func wbDocFollowStyle(raw json.RawMessage, st *webbuilder.SiteSettings) json.RawMessage {
	if st == nil || len(raw) == 0 {
		return raw
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return raw
	}
	var site map[string]any
	if json.Unmarshal(doc["site"], &site) != nil || site == nil || site["global"] != true {
		return raw
	}
	var style map[string]any
	b, _ := json.Marshal(st)
	_ = json.Unmarshal(b, &style)
	for _, k := range wbSiteStyleKeys {
		if v, ok := style[k]; ok {
			site[k] = v
		} else {
			delete(site, k)
		}
	}
	sb, err := json.Marshal(site)
	if err != nil {
		return raw
	}
	doc["site"] = sb
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// wbOutdatedWhere is the condition of a web_builder_docs row d whose published markup has to be
// drawn again: an older renderer drew it, or it follows the site style and its stored look differs
// from the current one. It uses $1 (the renderer version) and $2 (the style as JSON, or NULL).
func (s *Server) wbOutdatedWhere() (string, []any) {
	var style *string
	if st := s.wbSiteStyle(); st != nil {
		b, _ := json.Marshal(st)
		v := string(b)
		style = &v
	}
	var diff []string
	for _, k := range wbSiteStyleKeys {
		diff = append(diff, fmt.Sprintf("(d.doc->'site'->'%s') IS DISTINCT FROM ($2::jsonb->'%s')", k, k))
	}
	where := `(d.renderer < $1 OR ($2::jsonb IS NOT NULL AND d.doc->'site'->>'global' = 'true' AND (` + strings.Join(diff, " OR ") + `)))`
	return where, []any{webbuilder.RendererVersion, style}
}

// wbSiteStyleFollowers counts the designs that follow the site style and still have the old look.
func (s *Server) wbSiteStyleFollowers(ctx context.Context) int {
	where, args := s.wbOutdatedWhere()
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM web_builder_docs d JOIN posts p ON p.id = d.post_id
		WHERE d.renderer >= $1 AND `+where+` AND p.status NOT IN ('trash', 'auto-draft')`, args...).Scan(&n)
	return n
}
