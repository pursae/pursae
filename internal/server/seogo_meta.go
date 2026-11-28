package server

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Pursae SEO post and term meta. Every field has its own _seogo_* key; reads fall back to the keys
// Yoast SEO (_yoast_wpseo_*) and Rank Math (rank_math_*) store, so content imported from a WordPress
// site keeps its SEO data without a conversion step. seogoImportMeta copies them over for good.

// seogoFields lists the logical fields with their Yoast and Rank Math keys.
var seogoFields = map[string][2]string{
	"title":             {"_yoast_wpseo_title", "rank_math_title"},
	"description":       {"_yoast_wpseo_metadesc", "rank_math_description"},
	"focus":             {"_yoast_wpseo_focuskw", "rank_math_focus_keyword"},
	"canonical":         {"_yoast_wpseo_canonical", "rank_math_canonical_url"},
	"noindex":           {"_yoast_wpseo_meta-robots-noindex", "rank_math_robots"},
	"nofollow":          {"_yoast_wpseo_meta-robots-nofollow", "rank_math_robots"},
	"robots_adv":        {"_yoast_wpseo_meta-robots-adv", "rank_math_robots"},
	"og_title":          {"_yoast_wpseo_opengraph-title", "rank_math_facebook_title"},
	"og_description":    {"_yoast_wpseo_opengraph-description", "rank_math_facebook_description"},
	"og_image":          {"_yoast_wpseo_opengraph-image", "rank_math_facebook_image"},
	"tw_title":          {"_yoast_wpseo_twitter-title", "rank_math_twitter_title"},
	"tw_description":    {"_yoast_wpseo_twitter-description", "rank_math_twitter_description"},
	"tw_image":          {"_yoast_wpseo_twitter-image", "rank_math_twitter_image"},
	"cornerstone":       {"_yoast_wpseo_is_cornerstone", "rank_math_pillar_content"},
	"bc_title":          {"_yoast_wpseo_bctitle", "rank_math_breadcrumb_title"},
	"schema_page":       {"_yoast_wpseo_schema_page_type", ""},
	"schema_article":    {"_yoast_wpseo_schema_article_type", ""},
	"schema_custom":     {"", ""},
	"primary_category":  {"_yoast_wpseo_primary_category", "rank_math_primary_category"},
	"max_snippet":       {"", "rank_math_advanced_robots"},
	"max_image_preview": {"", "rank_math_advanced_robots"},
	"max_video_preview": {"", "rank_math_advanced_robots"},
	"redirect":          {"_yoast_wpseo_redirect", ""},
	"seo_score":         {"_yoast_wpseo_linkdex", "rank_math_seo_score"},
	"read_score":        {"_yoast_wpseo_content_score", ""},
}

func seogoKey(field string) string { return "_seogo_" + field }

func seogoRaw(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		if t {
			return "1", true
		}
		return "", true
	case []any:
		var parts []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ","), true
	}
	b, _ := json.Marshal(v)
	return string(b), true
}

var phpStrRe = regexp.MustCompile(`s:\d+:"([^"]*)"`)

// phpList reads the values of a serialized PHP array (a:2:{i:0;s:5:"index";…}) or a comma list.
func phpList(v string) []string {
	if strings.HasPrefix(v, "a:") {
		var out []string
		for _, m := range phpStrRe.FindAllStringSubmatch(v, -1) {
			out = append(out, m[1])
		}
		return out
	}
	var out []string
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// phpMap reads the key/value pairs of a serialized PHP array (Rank Math's advanced robots).
func phpMap(v string) map[string]string {
	out := map[string]string{}
	if !strings.HasPrefix(v, "a:") {
		return out
	}
	re := regexp.MustCompile(`s:\d+:"([^"]*)";(?:s:\d+:"([^"]*)"|i:(-?\d+))`)
	for _, m := range re.FindAllStringSubmatch(v, -1) {
		out[m[1]] = m[2] + m[3]
	}
	return out
}

// seogoMeta reads one logical field: Pursae SEO's own key first, then Yoast's, then Rank Math's.
func seogoMeta(m map[string]any, field string) string {
	if v, ok := seogoRaw(m, seogoKey(field)); ok {
		switch field {
		case "noindex", "og_image", "tw_image":
			return seogoNormalize(field, v) // rows saved before these values were normalized
		}
		return v
	}
	keys := seogoFields[field]
	if keys[0] != "" {
		if v, ok := seogoRaw(m, keys[0]); ok && v != "" && !(field == "robots_adv" && v == "none") {
			switch field {
			case "noindex": // 0 default, 1 noindex, 2 index
				return map[string]string{"1": "noindex", "2": "index"}[v]
			case "nofollow":
				return map[string]string{"1": "1"}[v]
			case "focus":
				if extra, ok := seogoRaw(m, "_yoast_wpseo_focuskeywords"); ok && extra != "" {
					var list []struct {
						Keyword string `json:"keyword"`
					}
					if json.Unmarshal([]byte(extra), &list) == nil {
						for _, k := range list {
							if k.Keyword != "" {
								v += "," + k.Keyword
							}
						}
					}
				}
			case "seo_score", "read_score":
				return v
			}
			return yoastVars(v)
		}
	}
	if keys[1] != "" {
		if v, ok := seogoRaw(m, keys[1]); ok && v != "" {
			switch field {
			case "noindex":
				for _, d := range phpList(v) {
					if d == "noindex" || d == "index" {
						return d
					}
				}
				return ""
			case "nofollow":
				for _, d := range phpList(v) {
					if d == "nofollow" {
						return "1"
					}
				}
				return ""
			case "robots_adv":
				var keep []string
				for _, d := range phpList(v) {
					if d == "noarchive" || d == "nosnippet" || d == "noimageindex" {
						keep = append(keep, d)
					}
				}
				return strings.Join(keep, ",")
			case "max_snippet", "max_image_preview", "max_video_preview":
				return phpMap(v)[strings.ReplaceAll(field, "_", "-")]
			case "cornerstone":
				if v == "on" {
					return "1"
				}
				return ""
			}
			return v
		}
	}
	return ""
}

// yoastVars rewrites Yoast's %%var%% syntax into Pursae SEO's %var% (both are accepted anyway).
func yoastVars(v string) string {
	if !strings.Contains(v, "%%") {
		return v
	}
	return yoastVarRe.ReplaceAllString(v, "%$1%")
}

var yoastVarRe = regexp.MustCompile(`%%([a-zA-Z0-9_]+)%%`)

// seogoSetMeta writes one field under Pursae SEO's key ("" removes it so the fallbacks apply again
// only when no Yoast/Rank Math value exists; otherwise an empty override is stored).
func seogoSetMeta(m map[string]any, field, value string) {
	value = seogoNormalize(field, strings.TrimSpace(value))
	if value == "" {
		keys := seogoFields[field]
		has := false
		for _, k := range keys {
			if k != "" {
				if v, ok := seogoRaw(m, k); ok && v != "" {
					has = true
				}
			}
		}
		if !has {
			delete(m, seogoKey(field))
			return
		}
	}
	m[seogoKey(field)] = value
}

// seogoImportMeta copies Yoast SEO and Rank Math meta into Pursae SEO's keys (WXR import, Tools › Import).
// The original keys stay so that switching back to WordPress loses nothing. It reports how many
// fields it filled.
func seogoImportMeta(m map[string]any) int {
	if m == nil {
		return 0
	}
	n := 0
	for field := range seogoFields {
		if _, ok := m[seogoKey(field)]; ok {
			continue
		}
		if v := seogoMeta(m, field); v != "" {
			m[seogoKey(field)] = v
			n++
		}
	}
	return n
}

// seogoFocusList splits the focus keyphrases: the first is the primary one.
func seogoFocusList(v string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range strings.Split(v, ",") {
		k = strings.TrimSpace(k)
		if k != "" && !seen[strings.ToLower(k)] {
			seen[strings.ToLower(k)] = true
			out = append(out, k)
		}
	}
	return out
}

// seogoNormalize maps the values API clients send for a field onto the ones the front end reads:
// noindex takes "noindex"/"index" (a truthy "1"/"true" meant noindex but was ignored), and og_image
// must be an http(s) or site-relative URL (it lands in og:image and JSON-LD; javascript: is dropped).
func seogoNormalize(field, v string) string {
	switch field {
	case "noindex":
		switch strings.ToLower(v) {
		case "noindex", "1", "true", "yes", "on":
			return "noindex"
		case "index":
			return "index"
		case "0", "false", "no", "off":
			return ""
		}
	case "nofollow":
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on", "nofollow":
			return "1"
		case "0", "false", "no", "off":
			return ""
		}
	case "og_image", "tw_image":
		if v != "" && !seogoSafeURL(v) {
			return ""
		}
	}
	return v
}

// seogoSafeURL reports whether v is an absolute http(s) URL or a site-relative path.
func seogoSafeURL(v string) bool {
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.Host != ""
	case "":
		return strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") || u.Host == "" && !strings.Contains(v, ":")
	}
	return false
}
