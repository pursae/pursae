package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/tetratelabs/wazero/api"
)

// hostModuleName is the import module plugins link against.
const hostModuleName = "wpgo"

// Host-function error codes, sent to the guest in failed responses.
const (
	CodePermissionDenied = "permission_denied"
	CodeInvalidRequest   = "invalid_request"
	CodeHostError        = "host_error"
)

// Request limits enforced before the Host is called.
const (
	maxOptionName    = 191 // WordPress wp_options.option_name length
	maxStorageKey    = 256
	maxStorageValue  = 1 << 20
	maxNoticeMessage = 2000
	maxCallbackName  = 128
	maxURLLength     = 2048
)

// CallError is a host-function failure reported to the guest.
type CallError struct {
	Code    string
	Message string
}

func (e *CallError) Error() string { return e.Code + ": " + e.Message }

func invalid(format string, args ...any) error {
	return &CallError{Code: CodeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

// hostResponse is the envelope of every host-function response.
type hostResponse struct {
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Code   string `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
}

// hostHandler serves one host function for plugin p with the raw request.
type hostHandler func(ctx context.Context, p *Plugin, req []byte) (any, error)

// instantiateHostModule defines the "wpgo" import module shared by all
// plugins. Every function has the signature (ptr, len i32) -> i64 and returns
// a packed ptr<<32|len of a JSON response envelope allocated in guest memory
// with wpgo_alloc, or 0 if no response could be delivered.
func (rt *Runtime) instantiateHostModule(ctx context.Context) error {
	b := rt.wz.NewHostModuleBuilder(hostModuleName)
	fns := []struct {
		name string
		perm Permission
		h    hostHandler
	}{
		{"add_hook", "", hostAddHook},
		{"get_option", PermOptionsRead, hostGetOption},
		{"update_option", PermOptionsWrite, hostUpdateOption},
		{"log", "", hostLog},
		{"http_get", PermHTTPOutbound, hostHTTPGet},
		{"storage_get", PermStorage, hostStorageGet},
		{"storage_set", PermStorage, hostStorageSet},
		{"add_admin_notice", PermAdminNotices, hostAdminNotice},
		// ABI 1 additions (all additive; see README "Host imports").
		{"register_shortcode", PermContentFilter, hostRegisterShortcode},
		{"register_rest_route", PermRestRoutes, hostRegisterRestRoute},
		{"get_post", PermPostsRead, hostGetPost},
		{"query_posts", PermPostsRead, hostQueryPosts},
		{"insert_post", PermPostsWrite, hostInsertPost},
		{"update_post", PermPostsWrite, hostUpdatePost},
		{"get_current_user", PermUsersRead, hostGetCurrentUser},
		{"schedule_event", PermCron, hostScheduleEvent},
		{"unschedule_event", PermCron, hostUnscheduleEvent},
		{"register_settings_page", PermAdminSettings, hostRegisterSettingsPage},
		{"transient_get", PermStorage, hostTransientGet},
		{"transient_set", PermStorage, hostTransientSet},
		{"transient_delete", PermStorage, hostTransientDelete},
		{"enqueue_style", PermContentFilter, hostEnqueueStyle},
		{"enqueue_script", PermContentFilter, hostEnqueueScript},
	}
	for _, f := range fns {
		b.NewFunctionBuilder().
			WithFunc(rt.wrapHost(f.name, f.perm, f.h)).
			WithParameterNames("ptr", "len").
			Export(f.name)
	}
	if _, err := b.Instantiate(ctx); err != nil {
		return fmt.Errorf("plugins: instantiate host module: %w", err)
	}
	return nil
}

// wrapHost adapts a hostHandler to the wazero calling convention, adding
// request decoding, the permission check, panic safety and response encoding.
func (rt *Runtime) wrapHost(name string, perm Permission, h hostHandler) func(context.Context, api.Module, uint32, uint32) uint64 {
	return func(ctx context.Context, m api.Module, ptr, size uint32) (packed uint64) {
		p := rt.pluginForModule(m.Name())
		if p == nil {
			return 0
		}
		defer func() {
			if r := recover(); r != nil {
				p.logs.add("error", "runtime", fmt.Sprintf("host function %s panicked: %v", name, r))
				packed = respond(ctx, p, m, nil, &CallError{Code: CodeHostError, Message: "internal error"})
			}
		}()
		if size > rt.cfg.MaxMessageSize {
			return respond(ctx, p, m, nil, invalid("request of %d bytes exceeds the limit", size))
		}
		view, ok := m.Memory().Read(ptr, size)
		if !ok {
			return respond(ctx, p, m, nil, invalid("request is out of memory bounds"))
		}
		req := append([]byte(nil), view...)
		if perm != "" && !p.perms.Has(perm) {
			p.logs.add("warning", "runtime", fmt.Sprintf("denied %s: missing permission %q", name, perm))
			return respond(ctx, p, m, nil, &CallError{
				Code:    CodePermissionDenied,
				Message: fmt.Sprintf("plugin %s lacks the %q permission required by %s", p.Slug(), perm, name),
			})
		}
		result, err := h(ctx, p, req)
		return respond(ctx, p, m, result, err)
	}
}

// respond encodes the envelope, copies it into guest memory and returns the
// packed pointer.
func respond(ctx context.Context, p *Plugin, m api.Module, result any, err error) uint64 {
	env := hostResponse{OK: err == nil, Result: result}
	if err != nil {
		var ce *CallError
		if errors.As(err, &ce) {
			env.Code, env.Error = ce.Code, ce.Message
		} else {
			env.Code, env.Error = CodeHostError, err.Error()
		}
	}
	body, mErr := json.Marshal(env)
	if mErr != nil {
		body, _ = json.Marshal(hostResponse{Code: CodeHostError, Error: "cannot encode response"})
	}
	ptr, wErr := writeGuest(ctx, m, body)
	if wErr != nil {
		p.logs.add("error", "runtime", "cannot deliver host response: "+wErr.Error())
		return 0
	}
	return uint64(ptr)<<32 | uint64(len(body))
}

// writeGuest allocates len(data) bytes with wpgo_alloc and copies data in.
func writeGuest(ctx context.Context, m api.Module, data []byte) (uint32, error) {
	res, err := m.ExportedFunction(exportAlloc).Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, fmt.Errorf("wpgo_alloc: %w", err)
	}
	ptr := uint32(res[0])
	if ptr == 0 && len(data) > 0 {
		return 0, errors.New("wpgo_alloc returned null")
	}
	if !m.Memory().Write(ptr, data) {
		return 0, errors.New("wpgo_alloc returned an out-of-bounds pointer")
	}
	return ptr, nil
}

// readGuest copies a packed ptr/len buffer out of guest memory, optionally
// releasing it with wpgo_free.
func readGuest(ctx context.Context, m api.Module, packed uint64, limit uint32, free bool) ([]byte, error) {
	ptr, size := uint32(packed>>32), uint32(packed)
	if size > limit {
		return nil, fmt.Errorf("response of %d bytes exceeds the %d byte limit", size, limit)
	}
	view, ok := m.Memory().Read(ptr, size)
	if !ok {
		return nil, errors.New("response is out of memory bounds")
	}
	out := append([]byte(nil), view...)
	if free {
		if _, err := m.ExportedFunction(exportFree).Call(ctx, uint64(ptr), uint64(size)); err != nil {
			return nil, fmt.Errorf("wpgo_free: %w", err)
		}
	}
	return out, nil
}

// decodeRequest unmarshals a request, mapping errors to invalid_request.
func decodeRequest(req []byte, v any) error {
	if err := json.Unmarshal(req, v); err != nil {
		return invalid("malformed request: %v", err)
	}
	return nil
}

func hostAddHook(_ context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Type     string `json:"type"`
		Hook     string `json:"hook"`
		Priority *int   `json:"priority"`
		Callback string `json:"callback"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if r.Type != "filter" && r.Type != "action" {
		return nil, invalid(`type must be "filter" or "action", got %q`, r.Type)
	}
	if r.Callback == "" || len(r.Callback) > maxCallbackName {
		return nil, invalid("callback name must be 1-%d characters", maxCallbackName)
	}
	if !p.manifest.DeclaresHook(r.Hook) {
		p.logs.add("warning", "runtime", fmt.Sprintf("refused hook %q: not declared in plugin.json", r.Hook))
		return nil, &CallError{Code: CodePermissionDenied, Message: fmt.Sprintf("hook %q is not declared in plugin.json \"hooks\"", r.Hook)}
	}
	if need := HookPermission(r.Hook); need != "" && !p.perms.Has(need) {
		p.logs.add("warning", "runtime", fmt.Sprintf("refused hook %q: missing permission %q", r.Hook, need))
		return nil, &CallError{Code: CodePermissionDenied, Message: fmt.Sprintf("hook %q requires the %q permission", r.Hook, need)}
	}
	priority := DefaultPriority
	if r.Priority != nil {
		priority = *r.Priority
	}
	return nil, p.registerHook(RegisteredHook{Type: r.Type, Hook: r.Hook, Priority: priority, Callback: r.Callback})
}

func checkOptionName(name string) error {
	if name == "" || len(name) > maxOptionName {
		return invalid("option name must be 1-%d characters", maxOptionName)
	}
	return nil
}

func hostGetOption(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Name string `json:"name"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if err := checkOptionName(r.Name); err != nil {
		return nil, err
	}
	v, found, err := p.host.GetOption(ctx, p.Slug(), r.Name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"found": found, "value": v}, nil
}

func hostUpdateOption(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Name  string `json:"name"`
		Value any    `json:"value"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if err := checkOptionName(r.Name); err != nil {
		return nil, err
	}
	return nil, p.host.UpdateOption(ctx, p.Slug(), r.Name, r.Value)
}

func hostLog(_ context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	switch r.Level {
	case "debug", "info", "warning", "error":
	case "warn":
		r.Level = "warning"
	default:
		r.Level = "info"
	}
	p.logs.add(r.Level, "plugin", r.Message)
	return nil, nil
}

func hostHTTPGet(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		URL string `json:"url"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	u, err := url.Parse(r.URL)
	if err != nil || len(r.URL) > maxURLLength || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, invalid("url must be an absolute http(s) URL of at most %d characters", maxURLLength)
	}
	resp, err := p.host.HTTPGet(ctx, p.Slug(), u.String())
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("host returned no response")
	}
	if len(resp.Body) > p.rt.cfg.MaxHTTPBody {
		return nil, &CallError{Code: CodeHostError, Message: fmt.Sprintf("response body exceeds %d bytes", p.rt.cfg.MaxHTTPBody)}
	}
	return resp, nil
}

func checkStorageKey(key string) error {
	if key == "" || len(key) > maxStorageKey {
		return invalid("storage key must be 1-%d characters", maxStorageKey)
	}
	return nil
}

func hostStorageGet(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Key string `json:"key"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if err := checkStorageKey(r.Key); err != nil {
		return nil, err
	}
	v, found, err := p.host.StorageGet(ctx, p.Slug(), r.Key)
	if err != nil {
		return nil, err
	}
	return map[string]any{"found": found, "value": v}, nil
}

func hostStorageSet(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	if err := checkStorageKey(r.Key); err != nil {
		return nil, err
	}
	if len(r.Value) > maxStorageValue {
		return nil, invalid("storage value exceeds %d bytes", maxStorageValue)
	}
	return nil, p.host.StorageSet(ctx, p.Slug(), r.Key, r.Value)
}

func hostAdminNotice(ctx context.Context, p *Plugin, req []byte) (any, error) {
	var r struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := decodeRequest(req, &r); err != nil {
		return nil, err
	}
	switch r.Type {
	case "":
		r.Type = "info"
	case "info", "success", "warning", "error":
	default:
		return nil, invalid(`notice type must be info, success, warning or error, got %q`, r.Type)
	}
	r.Message = strings.TrimSpace(r.Message)
	if r.Message == "" || len(r.Message) > maxNoticeMessage {
		return nil, invalid("notice message must be 1-%d characters", maxNoticeMessage)
	}
	return nil, p.host.AdminNotice(ctx, p.Slug(), Notice{Plugin: p.Slug(), Type: r.Type, Message: r.Message})
}
