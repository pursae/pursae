package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// Goocommerce's REST APIs: the Store API (wc/store/v1, WooCommerce Blocks' public cart / checkout /
// catalog API) and the management API (wc/v3, the WooCommerce REST API v3), with WooCommerce's JSON
// shapes, query parameters, error codes and headers so clients written for WooCommerce work unchanged.
//
// Every route answers 404 rest_no_route while the plugin is off. wc/v3 authenticates with the core
// REST auth (logged-in cookie + X-WP-Nonce, application passwords) or a consumer key / secret of
// goo_api_keys (goo_rest_auth.go); the Store API uses the storefront session with a Nonce header or a
// Cart-Token (goo_rest_store_cart.go).

const (
	gooStoreNS = "wc/store/v1"
	gooV3NS    = "wc/v3"
)

func (s *Server) registerGooREST() {
	s.registerGooStoreREST()
	s.registerGooStoreCartREST()
	s.registerGooV3Products()
	s.registerGooV3Orders()
	s.registerGooV3Misc()
	s.registerGooV3Modules()     // goo_rest_v3_modules.go
	s.registerGooAnalyticsREST() // goo_rest_analytics.go
}

// gooAPIErr is a WP_Error returned by an endpoint.
type gooAPIErr struct {
	Status  int
	Code    string
	Message string
}

func (e *gooAPIErr) Error() string { return e.Message }

func gooErr(status int, code, msg string) *gooAPIErr {
	return &gooAPIErr{Status: status, Code: code, Message: msg}
}

func gooFail(c *gin.Context, e *gooAPIErr) { restErr(c, e.Status, e.Code, e.Message) }

// gooErrJSON is a WP_Error as an item of a batch response.
func gooErrJSON(e *gooAPIErr) map[string]any {
	return map[string]any{"code": e.Code, "message": e.Message, "data": map[string]any{"status": e.Status}}
}

func gooNoRoute(c *gin.Context) {
	restErr(c, http.StatusNotFound, "rest_no_route", "No route was found matching the URL and request method.")
}

// gooRoute registers a route of a Goocommerce namespace; every handler 404s while the plugin is off.
func (s *Server) gooRoute(ns, pattern string, methods map[string]restHandler) {
	wrapped := make(map[string]restHandler, len(methods))
	for m, h := range methods {
		wrapped[m] = func(c *gin.Context, req *restRequest) {
			if !s.gooOn() {
				gooNoRoute(c)
				return
			}
			h(c, req)
		}
	}
	s.rest.register(ns, "/"+ns+pattern, wrapped, nil)
}

// wc/v3 permissions ------------------------------------------------------------------------------------

// gooCaps are the capabilities a wc/v3 resource needs to read, write (create / edit) and delete.
type gooCaps struct{ Read, Write, Delete string }

func gooCapsAll(cap string) gooCaps { return gooCaps{cap, cap, cap} }

// gooV3 registers a wc/v3 route: authentication (core REST auth or a consumer key), then the
// capability the method needs, like WooCommerce's permission callbacks.
func (s *Server) gooV3(pattern string, caps gooCaps, methods map[string]restHandler) {
	wrapped := make(map[string]restHandler, len(methods))
	for m, h := range methods {
		wrapped[m] = func(c *gin.Context, req *restRequest) {
			u, aerr := s.gooV3User(c, req.Method)
			if aerr != nil {
				gooFail(c, aerr)
				return
			}
			cap, verb, code := caps.Read, "list resources", "woocommerce_rest_cannot_view"
			switch {
			case strings.HasSuffix(pattern, "/batch"):
				cap, verb, code = caps.Write, "batch manipulate this resource", "woocommerce_rest_cannot_batch"
			case m == http.MethodPost && !strings.Contains(pattern, "(?P<id>"):
				cap, verb, code = caps.Write, "create resources", "woocommerce_rest_cannot_create"
			case m == http.MethodPost || m == http.MethodPut || m == http.MethodPatch:
				cap, verb, code = caps.Write, "edit this resource", "woocommerce_rest_cannot_edit"
			case m == http.MethodDelete:
				cap, verb, code = caps.Delete, "delete this resource", "woocommerce_rest_cannot_delete"
			case strings.Contains(pattern, "(?P<id>"):
				verb = "view this resource"
			}
			if u == nil {
				restErr(c, http.StatusUnauthorized, code, "Sorry, you cannot "+verb+".")
				return
			}
			if cap != "" && !u.Can(cap, nil) {
				restErr(c, http.StatusForbidden, code, "Sorry, you cannot "+verb+".")
				return
			}
			h(c, req)
		}
	}
	s.gooRoute(gooV3NS, pattern, wrapped)
}

// gooV3Actor is the user a wc/v3 request runs as.
func gooV3Actor(c *gin.Context) *store.User { return restCurrent(c) }

func gooActorID(c *gin.Context) int64 {
	if u := gooV3Actor(c); u != nil {
		return u.ID
	}
	return 0
}

// Batch ------------------------------------------------------------------------------------------------

// gooBatchOps are a resource's create / update / delete operations for …/batch.
type gooBatchOps struct {
	Create func(c *gin.Context, b map[string]any) (map[string]any, *gooAPIErr)
	Update func(c *gin.Context, id int64, b map[string]any) (map[string]any, *gooAPIErr)
	Delete func(c *gin.Context, id int64, force bool) (map[string]any, *gooAPIErr)
}

// gooBatchLimit is WooCommerce's woocommerce_rest_batch_items_limit.
const gooBatchLimit = 100

// gooBatch runs a batch request: {create: [...], update: [...], delete: [ids]}.
func (s *Server) gooBatch(c *gin.Context, req *restRequest, ops gooBatchOps) {
	creates, _ := req.Body["create"].([]any)
	updates, _ := req.Body["update"].([]any)
	deletes, _ := req.Body["delete"].([]any)
	if len(creates)+len(updates)+len(deletes) > gooBatchLimit {
		restErr(c, http.StatusRequestEntityTooLarge, "woocommerce_rest_request_entity_too_large", fmt.Sprintf("Unable to accept more than %d items for this request.", gooBatchLimit))
		return
	}
	out := map[string]any{}
	if len(creates) > 0 && ops.Create != nil {
		var res []any
		for _, x := range creates {
			b, _ := x.(map[string]any)
			if b == nil {
				b = map[string]any{}
			}
			obj, err := ops.Create(c, b)
			if err != nil {
				res = append(res, map[string]any{"id": 0, "error": gooErrJSON(err)})
				continue
			}
			res = append(res, obj)
		}
		out["create"] = res
	}
	if len(updates) > 0 && ops.Update != nil {
		var res []any
		for _, x := range updates {
			b, _ := x.(map[string]any)
			id := gooInt(b["id"])
			if b == nil || id == 0 {
				res = append(res, map[string]any{"id": id, "error": gooErrJSON(gooErr(400, "woocommerce_rest_invalid_id", "Invalid ID."))})
				continue
			}
			obj, err := ops.Update(c, id, b)
			if err != nil {
				res = append(res, map[string]any{"id": id, "error": gooErrJSON(err)})
				continue
			}
			res = append(res, obj)
		}
		out["update"] = res
	}
	if len(deletes) > 0 && ops.Delete != nil {
		var res []any
		for _, x := range deletes {
			id := gooInt(x)
			if m, ok := x.(map[string]any); ok {
				id = gooInt(m["id"])
			}
			obj, err := ops.Delete(c, id, true)
			if err != nil {
				res = append(res, map[string]any{"id": id, "error": gooErrJSON(err)})
				continue
			}
			res = append(res, obj)
		}
		out["delete"] = res
	}
	restJSON(c, http.StatusOK, out)
}

// Body value helpers -------------------------------------------------------------------------------------

// gooStr reads a JSON value as a string.
func gooStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func gooInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	}
	return 0
}

func gooFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

// gooBool reads true / "true" / "yes" / "1" / 1.
func gooBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		t = strings.ToLower(strings.TrimSpace(t))
		return t == "true" || t == "yes" || t == "1" || t == "on"
	case float64:
		return t != 0
	}
	return false
}

// gooAmount reads a price string ("" → nil).
func gooRESTAmount(v any) *float64 {
	if v == nil {
		return nil
	}
	if f, ok := gooFloat(v); ok {
		return &f
	}
	return nil
}

func gooList(v any) []any {
	l, _ := v.([]any)
	return l
}

func gooObj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// gooIDList reads [1,2] or [{"id":1},{"id":2}] or "1,2".
func gooIDList(v any) []int64 {
	var out []int64
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			if m, ok := x.(map[string]any); ok {
				if id := gooInt(m["id"]); id > 0 {
					out = append(out, id)
				}
				continue
			}
			if id := gooInt(x); id > 0 {
				out = append(out, id)
			}
		}
	case string:
		for _, p := range strings.Split(t, ",") {
			if id := gooInt(p); id > 0 {
				out = append(out, id)
			}
		}
	}
	return out
}

// gooQueryIDs reads an id list from the query string: include=1,2 or include[]=1&include[]=2.
func gooQueryIDs(c *gin.Context, key string) []int64 {
	vals := c.QueryArray(key + "[]")
	if v, ok := c.GetQuery(key); ok {
		vals = append(vals, strings.Split(v, ",")...)
	}
	var out []int64
	for _, v := range vals {
		if id := gooInt(v); id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// gooQueryList reads a string list: status=a,b or status[]=a&status[]=b.
func gooQueryList(c *gin.Context, key string) []string {
	vals := c.QueryArray(key + "[]")
	if v, ok := c.GetQuery(key); ok && v != "" {
		vals = append(vals, strings.Split(v, ",")...)
	}
	var out []string
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// gooQueryBool reads a boolean query argument; ok is false when absent.
func gooQueryBool(c *gin.Context, key string) (bool, bool) {
	v, ok := c.GetQuery(key)
	if !ok || v == "" {
		return false, false
	}
	return gooBool(v), true
}

// gooPage reads page / per_page (WooCommerce: per_page 1..100, default 10) and offset.
func gooPage(c *gin.Context) (page, per, offset int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	// Bounded like store.MaxPage: (page-1)*per and offset+per must not overflow into a negative
	// offset or limit (a panic on in-memory lists, the whole table read for SQL ones).
	page = min(max(page, 1), store.MaxPage)
	per, _ = strconv.Atoi(c.DefaultQuery("per_page", "10"))
	if per == -1 {
		per = 100
	}
	if per < 1 {
		per = 10
	}
	if per > 100 {
		per = 100
	}
	offset = -1
	if v, ok := c.GetQuery("offset"); ok {
		offset, _ = strconv.Atoi(v)
		offset = min(offset, store.MaxPage*100)
	}
	return
}

// gooPaginateSlice pages an in-memory list and sets the pagination headers.
func gooPaginateSlice[T any](c *gin.Context, all []T) []T {
	page, per, offset := gooPage(c)
	start := (page - 1) * per
	if offset >= 0 {
		start = offset
	}
	paginate(c, len(all), per, page)
	if start >= len(all) {
		return []T{}
	}
	end := min(start+per, len(all))
	return all[start:end]
}

// Dates ----------------------------------------------------------------------------------------------

const gooDateFmt = "2006-01-02T15:04:05"

// gooDate is a date in the site's time zone, WooCommerce's "2017-03-23T17:01:14" (nil for none).
func (s *Server) gooDate(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.In(s.loc()).Format(gooDateFmt)
}

func gooDateGMT(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(gooDateFmt)
}

// gooParseDate reads an ISO 8601 date; without a zone it is in the site's zone (gmt=false) or UTC.
func (s *Server) gooParseDate(v string, gmt bool) (*time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05.000Z07:00"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t, true
		}
	}
	loc := s.loc()
	if gmt {
		loc = time.UTC
	}
	for _, layout := range []string{gooDateFmt, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return &t, true
		}
	}
	return nil, false
}

// gooBodyDate reads key or key_gmt from a body; set is false when neither is present.
func (s *Server) gooBodyDate(b map[string]any, key string) (t *time.Time, set bool, err *gooAPIErr) {
	for _, k := range []string{key + "_gmt", key} {
		raw, ok := b[k]
		if !ok {
			continue
		}
		if raw == nil {
			return nil, true, nil
		}
		v, ok2 := s.gooParseDate(gooStr(raw), k != key)
		if !ok2 {
			return nil, true, gooErr(400, "rest_invalid_param", "Invalid parameter(s): "+k)
		}
		return v, true, nil
	}
	return nil, false, nil
}

// Money -----------------------------------------------------------------------------------------------

// gooDec is wc_format_decimal($v, $dp): the store's decimals.
func (s *Server) gooDec(v float64) string { return gooFormatDecimal(v, s.gooDecimals()) }

// gooDecPtr formats an optional price as WooCommerce stores it ("" for none).
func gooDecPtr(v *float64) string {
	if v == nil {
		return ""
	}
	return gooFormatDecimal(*v, -1)
}

// Meta data -----------------------------------------------------------------------------------------------

// gooMetaJSON lists public meta as [{id, key, value}] (keys starting with "_" are private).
func gooMetaJSON(meta map[string]any) []any {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		if !strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for i, k := range keys {
		out = append(out, map[string]any{"id": i + 1, "key": k, "value": meta[k]})
	}
	return out
}

// gooApplyMeta applies meta_data [{key, value}] (a null value deletes the key).
func gooApplyMeta(meta map[string]any, list any) {
	for _, x := range gooList(list) {
		m := gooObj(x)
		k := gooStr(m["key"])
		if k == "" || gooProtectedPostMetaKey(k) {
			continue
		}
		if v, ok := m["value"]; !ok || v == nil {
			delete(meta, k)
		} else {
			meta[k] = v
		}
	}
}

// gooProtectedPostMetaKey reports whether k is core-internal post meta that REST meta_data may not
// write: Web Builder keys (the "_wb_html_" values are printed as trusted HTML), builder documents and
// the page template. Ordinary "_" integration meta stays writable.
func gooProtectedPostMetaKey(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	if strings.HasPrefix(k, webbuilder.TrustedHTMLField) {
		return true
	}
	for _, p := range []string{"_wb_", "_web_builder", "_elementor_", "_wp_page_template"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// gooAddressJSON is a billing / shipping address with every WooCommerce key present.
func gooAddressJSON(a map[string]string, billing bool) map[string]any {
	keys := []string{"first_name", "last_name", "company", "address_1", "address_2", "city", "state", "postcode", "country", "phone"}
	if billing {
		keys = append(keys, "email")
	}
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		out[k] = a[k]
	}
	return out
}

// gooApplyAddress copies an address object of a request into a map.
func gooApplyAddress(dst map[string]string, v any) {
	for k, x := range gooObj(v) {
		if containsStr(gooAddressKeys, k) {
			dst[k] = strings.TrimSpace(gooStr(x))
		}
	}
}

// gooHomeLink is a link to a REST route on this site.
func (s *Server) gooRESTURL(ctx context.Context, route string) string {
	return s.links(ctx).HomeURL("/wp-json/" + strings.TrimPrefix(route, "/"))
}

// gooLinks are a v3 object's _links.
func (s *Server) gooLinks(ctx context.Context, collection string, id int64) map[string]any {
	col := s.gooRESTURL(ctx, gooV3NS+"/"+collection)
	return restLinks(col+"/"+itoa64(id), col, nil)
}
