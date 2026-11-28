package wpgo

import (
	"encoding/json"
	"fmt"
)

// GetOption returns a site option as a string. Non-string option values are
// returned JSON-encoded. A missing option yields "", false, nil.
// Requires the "options:read" permission.
func GetOption(name string) (string, bool, error) {
	raw, err := invokeHost("get_option", getOptionRequest(name))
	if err != nil {
		return "", false, err
	}
	if v, ok := fastString(raw, optionFoundPrefix, resultSuffix); ok { // fastpath.go
		return v, true, nil
	}
	if string(raw) == optionMissing {
		return "", false, nil
	}
	res, err := decodeResponse(raw)
	if err != nil {
		return "", false, err
	}
	var out struct {
		Found bool            `json:"found"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", false, fmt.Errorf("wpgo: decode get_option result: %w", err)
	}
	if !out.Found {
		return "", false, nil
	}
	var s string
	if json.Unmarshal(out.Value, &s) == nil {
		return s, true, nil
	}
	return string(out.Value), true, nil
}

// GetOptionJSON decodes a site option into v and reports whether it exists.
// Requires the "options:read" permission.
func GetOptionJSON(name string, v any) (bool, error) {
	res, err := hostCall("get_option", map[string]string{"name": name})
	if err != nil {
		return false, err
	}
	var out struct {
		Found bool            `json:"found"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return false, fmt.Errorf("wpgo: decode get_option result: %w", err)
	}
	if !out.Found {
		return false, nil
	}
	return true, json.Unmarshal(out.Value, v)
}

// OptionBool interprets a site option as a boolean ("1", "true", "yes", "on"
// and JSON true are true). It returns def when the option is missing or the
// call fails. Requires the "options:read" permission.
func OptionBool(name string, def bool) bool {
	s, found, err := GetOption(name)
	if err != nil || !found {
		return def
	}
	switch s {
	case "1", "true", "yes", "on":
		return true
	case "", "0", "false", "no", "off":
		return false
	}
	return def
}

// UpdateOption stores a site option. value must be JSON-encodable.
// Requires the "options:write" permission.
func UpdateOption(name string, value any) error {
	_, err := hostCall("update_option", map[string]any{"name": name, "value": value})
	return err
}

// Log writes a message to the plugin's log, visible to administrators.
// level is one of "debug", "info", "warning", "error". Logging needs no
// permission; failures are ignored.
func Log(level, message string) {
	_, _ = hostCall("log", map[string]string{"level": level, "message": message})
}

// Logf is Log at "info" level with fmt.Sprintf formatting.
func Logf(format string, args ...any) { Log("info", fmt.Sprintf(format, args...)) }

// HTTPResponse is the result of HTTPGet.
type HTTPResponse struct {
	Status int               `json:"status"`
	Header map[string]string `json:"header,omitempty"`
	Body   string            `json:"body"`
}

// HTTPGet performs an outbound GET request through the host, which may apply
// its own allow-lists, timeouts and size limits.
// Requires the "http:outbound" permission.
func HTTPGet(url string) (*HTTPResponse, error) {
	res, err := hostCall("http_get", map[string]string{"url": url})
	if err != nil {
		return nil, err
	}
	var out HTTPResponse
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("wpgo: decode http_get result: %w", err)
	}
	return &out, nil
}

// HTTPRequestArgs describes an outbound request for HTTPRequest.
type HTTPRequestArgs struct {
	Method  string            `json:"method"` // GET (default), POST, PUT, PATCH, DELETE or HEAD
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"` // at most 32; Host, Content-Length and hop-by-hop headers are refused
	Body    string            `json:"body,omitempty"`    // at most 1 MiB
}

// HTTPRequest performs an outbound request with any allowed method, headers
// and body (the wp_remote_post / wp_remote_request equivalent).
// Requires the "http:outbound" permission.
func HTTPRequest(args HTTPRequestArgs) (*HTTPResponse, error) {
	res, err := hostCall("http_request", args)
	if err != nil {
		return nil, err
	}
	var out HTTPResponse
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("wpgo: decode http_request result: %w", err)
	}
	return &out, nil
}

// Mail is a message for SendMail.
type Mail struct {
	To      []string `json:"to"` // 1-10 addresses
	Subject string   `json:"subject"`
	Body    string   `json:"body"`           // plain text
	HTML    string   `json:"html,omitempty"` // optional HTML alternative
	ReplyTo string   `json:"reply_to,omitempty"`
}

// SendMail queues an e-mail through the site's mail settings (wp_mail).
// The sender is set by the site. Requires the "mail:send" permission.
func SendMail(m Mail) error {
	_, err := hostCall("send_mail", m)
	return err
}

// Notice types accepted by AdminNotice.
const (
	NoticeInfo    = "info"
	NoticeSuccess = "success"
	NoticeWarning = "warning"
	NoticeError   = "error"
)

// AdminNotice queues a notice for the admin dashboard. kind is one of the
// Notice* constants. Requires the "admin:notices" permission.
func AdminNotice(kind, message string) error {
	_, err := hostCall("add_admin_notice", map[string]any{"type": kind, "message": message})
	return err
}

// KVStore is the plugin-private key/value store. Keys are scoped to the plugin
// by the host, so plugins cannot see each other's data.
type KVStore struct{}

// Storage is the plugin's private key/value store.
// Requires the "storage" permission.
var Storage KVStore

// Get returns the value stored under key and whether it exists.
func (KVStore) Get(key string) (string, bool, error) {
	res, err := hostCall("storage_get", map[string]string{"key": key})
	if err != nil {
		return "", false, err
	}
	var out struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", false, fmt.Errorf("wpgo: decode storage_get result: %w", err)
	}
	return out.Value, out.Found, nil
}

// Set stores value under key.
func (KVStore) Set(key, value string) error {
	_, err := hostCall("storage_set", map[string]string{"key": key, "value": value})
	return err
}

// Delete removes key (no error when it does not exist).
func (KVStore) Delete(key string) error {
	_, err := hostCall("storage_delete", map[string]string{"key": key})
	return err
}

// List returns the keys that start with prefix in ascending order, at most limit
// (default and maximum 500) starting after the key "after". next is non-empty
// when more keys follow: pass it as "after" to get the next page.
func (KVStore) List(prefix, after string, limit int) (keys []string, next string, err error) {
	res, err := hostCall("storage_list", map[string]any{"prefix": prefix, "after": after, "limit": limit})
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Keys []string `json:"keys"`
		Next string   `json:"next"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, "", fmt.Errorf("wpgo: decode storage_list result: %w", err)
	}
	return out.Keys, out.Next, nil
}

// hostCall marshals req, invokes the named host import and decodes the
// response envelope. The import itself is provided by abi_wasip1.go (or a stub
// on other platforms).
func hostCall(name string, req any) (json.RawMessage, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("wpgo: encode %s request: %w", name, err)
	}
	raw, err := invokeHost(name, body)
	if err != nil {
		return nil, err
	}
	return decodeResponse(raw)
}
