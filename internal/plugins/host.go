package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Host is implemented by the core to serve plugin host-function calls. The
// runtime checks permissions, validates and size-limits requests before
// calling it, so implementations only deal with well-formed input. plugin is
// the calling plugin's slug.
//
// ctx carries the runtime's per-call deadline; implementations must honour it
// and should pass it on to any hooks they fire, so that the runtime can detect
// (and skip) re-entrant calls into the same plugin.
type Host interface {
	// GetOption returns a site option. Implementations may hide sensitive
	// options from plugins by reporting them as not found.
	GetOption(ctx context.Context, plugin, name string) (value any, found bool, err error)
	// UpdateOption creates or updates a site option. value is JSON-decoded
	// (string, float64, bool, nil, []any or map[string]any).
	UpdateOption(ctx context.Context, plugin, name string, value any) error
	// HTTPGet performs an outbound GET request. Implementations should apply
	// destination allow-lists (e.g. block private networks) and size limits.
	HTTPGet(ctx context.Context, plugin, url string) (*HTTPResponse, error)
	// StorageGet reads from the plugin-private key/value store.
	StorageGet(ctx context.Context, plugin, key string) (value string, found bool, err error)
	// StorageSet writes to the plugin-private key/value store.
	StorageSet(ctx context.Context, plugin, key, value string) error
	// AdminNotice queues a dashboard notice. Message is plain text and must
	// be escaped by the core when rendered.
	AdminNotice(ctx context.Context, plugin string, notice Notice) error

	// GetPost returns a post by id, or nil when it does not exist or the
	// plugin may not see it.
	GetPost(ctx context.Context, plugin string, id int64) (*PostData, error)
	// QueryPosts lists posts. q.PerPage is at most MaxQueryPerPage.
	QueryPosts(ctx context.Context, plugin string, q PostQuery) (*PostList, error)
	// InsertPost creates a post and returns it. Implementations apply the
	// same sanitisation as the admin (content is treated as untrusted HTML).
	InsertPost(ctx context.Context, plugin string, in PostInput) (*PostData, error)
	// UpdatePost changes the set fields of a post and returns the result.
	UpdatePost(ctx context.Context, plugin string, id int64, in PostInput) (*PostData, error)
	// CurrentUser returns the user the current request runs as, or nil when
	// there is none (anonymous visitor, cron, CLI).
	CurrentUser(ctx context.Context, plugin string) (*UserInfo, error)
	// ScheduleEvent schedules a recurring (Interval > 0) or one-off event.
	// When it fires, the core runs the hook named ev.Hook as an action with
	// ev.Args as its single argument. Scheduling the same hook again
	// replaces the earlier schedule.
	ScheduleEvent(ctx context.Context, plugin string, ev ScheduledEvent) error
	// UnscheduleEvent removes the plugin's schedule for hook.
	UnscheduleEvent(ctx context.Context, plugin, hook string) error
	// TransientGet reads a plugin-scoped transient; expired values are
	// reported as not found.
	TransientGet(ctx context.Context, plugin, key string) (value json.RawMessage, found bool, err error)
	// TransientSet stores a transient for ttl (0 means no expiry).
	TransientSet(ctx context.Context, plugin, key string, value json.RawMessage, ttl time.Duration) error
	// TransientDelete removes a transient.
	TransientDelete(ctx context.Context, plugin, key string) error
}

// MaxQueryPerPage caps PostQuery.PerPage.
const MaxQueryPerPage = 100

// HTTPResponse is the result of Host.HTTPGet.
type HTTPResponse struct {
	Status int               `json:"status"`
	Header map[string]string `json:"header,omitempty"`
	Body   string            `json:"body"`
}

// Notice is an admin dashboard notice queued by a plugin.
type Notice struct {
	Plugin  string `json:"plugin"`
	Type    string `json:"type"` // info, success, warning or error
	Message string `json:"message"`
}

// PostData is the post object plugins receive from get_post, query_posts,
// insert_post and update_post.
type PostData struct {
	ID       int64                 `json:"id"`
	Title    string                `json:"title"`
	Content  string                `json:"content"`
	Excerpt  string                `json:"excerpt"`
	Status   string                `json:"status"`
	Type     string                `json:"type"`
	Slug     string                `json:"slug"`
	Date     string                `json:"date"`     // RFC 3339
	Modified string                `json:"modified"` // RFC 3339
	Author   int64                 `json:"author"`
	Link     string                `json:"link,omitempty"`
	Terms    map[string][]TermData `json:"terms"` // by taxonomy: category, post_tag
	Meta     map[string]any        `json:"meta,omitempty"`
}

// TermData is a term attached to a post.
type TermData struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Taxonomy string `json:"taxonomy"`
}

// PostQuery is the request of query_posts.
type PostQuery struct {
	Type    string `json:"type"`    // post (default), page, …
	Status  string `json:"status"`  // publish (default), draft, pending, private, future, any
	Search  string `json:"search"`  // full-text search
	Author  int64  `json:"author"`  // author user id
	Term    int64  `json:"term"`    // posts in this term id
	OrderBy string `json:"orderby"` // date (default), title, modified, id
	Order   string `json:"order"`   // DESC (default) or ASC
	PerPage int    `json:"per_page"`
	Page    int    `json:"page"`
}

// PostList is the result of query_posts.
type PostList struct {
	Posts []PostData `json:"posts"`
	Total int        `json:"total"`
	Pages int        `json:"pages"`
}

// PostInput carries the fields of insert_post and update_post. Nil pointers
// are left unchanged on update and take defaults on insert (type post, status
// draft).
type PostInput struct {
	Title   *string        `json:"title,omitempty"`
	Content *string        `json:"content,omitempty"`
	Excerpt *string        `json:"excerpt,omitempty"`
	Status  *string        `json:"status,omitempty"`
	Type    *string        `json:"type,omitempty"`
	Slug    *string        `json:"slug,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"` // merged into the post's meta; a null value deletes a key
}

// UserInfo describes the current user (get_current_user).
type UserInfo struct {
	ID          int64    `json:"id"`
	Login       string   `json:"login"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
	Caps        []string `json:"caps"`
}

// ScheduledEvent is a cron registration (schedule_event).
type ScheduledEvent struct {
	Hook     string         `json:"hook"`           // action hook, must start with cron_
	Interval int64          `json:"interval"`       // seconds between runs; 0 for a one-off event
	At       int64          `json:"at"`             // unix time of the first (or only) run; 0 means now
	Args     map[string]any `json:"args,omitempty"` // passed to the action
}

// SettingsPage is a declarative admin settings page (register_settings_page).
type SettingsPage struct {
	PageTitle   string          `json:"page_title"`
	MenuTitle   string          `json:"menu_title"`
	Description string          `json:"description,omitempty"`
	Fields      []SettingsField `json:"fields"`
}

// SettingsField is one field of a SettingsPage. Its value is stored in the
// option plugin_<slug>_<id> (dashes in the slug become underscores).
type SettingsField struct {
	ID          string           `json:"id"`
	Label       string           `json:"label"`
	Type        string           `json:"type"` // text, textarea, number, checkbox, select
	Description string           `json:"description,omitempty"`
	Placeholder string           `json:"placeholder,omitempty"`
	Default     any              `json:"default,omitempty"`
	Options     []SettingsOption `json:"options,omitempty"` // select
	Min         *float64         `json:"min,omitempty"`     // number
	Max         *float64         `json:"max,omitempty"`     // number
	Rows        int              `json:"rows,omitempty"`    // textarea
}

// SettingsOption is a select choice.
type SettingsOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// OptionName returns the option a settings field is stored in.
func (f SettingsField) OptionName(plugin string) string { return SettingsOptionPrefix(plugin) + f.ID }

// SettingsOptionPrefix is the option prefix of a plugin's settings fields.
func SettingsOptionPrefix(plugin string) string {
	return "plugin_" + strings.ReplaceAll(plugin, "-", "_") + "_"
}

// Asset is a stylesheet or script a plugin enqueued from its package assets.
type Asset struct {
	Kind     string   `json:"kind"`   // style or script
	Handle   string   `json:"handle"` // unique per plugin
	Src      string   `json:"src"`    // path inside the package's assets/ directory
	Where    string   `json:"where"`  // front, admin or both
	Deps     []string `json:"deps,omitempty"`
	Media    string   `json:"media,omitempty"`     // style only
	InFooter bool     `json:"in_footer,omitempty"` // script only
}

// RestRequest is what the core sends a plugin REST route handler.
type RestRequest struct {
	Method  string            `json:"method"`
	Route   string            `json:"route"` // the matched path, e.g. /my-plugin/v1/items/7
	Params  map[string]string `json:"params"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    any               `json:"body"` // decoded JSON body, or a string for other content types
	User    *UserInfo         `json:"user"` // nil for anonymous requests
}

// RestResponse is what a plugin REST route handler returns.
type RestResponse struct {
	Status  int               `json:"status"`
	Body    json.RawMessage   `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// ErrHTTPDisabled is returned by MemoryHost.HTTPGet when no HTTP function is
// configured.
var ErrHTTPDisabled = errors.New("outbound HTTP is not configured")

// MemoryHost is an in-memory Host for tests and development.
type MemoryHost struct {
	// HTTP serves HTTPGet; nil makes HTTPGet fail with ErrHTTPDisabled.
	HTTP func(ctx context.Context, url string) (*HTTPResponse, error)
	// User is returned by CurrentUser; nil means anonymous.
	User *UserInfo
	// Now is the clock used for transients and post dates; nil uses time.Now.
	Now func() time.Time

	mu         sync.Mutex
	options    map[string]any
	storage    map[string]map[string]string
	notices    []Notice
	posts      map[int64]*PostData
	nextPost   int64
	events     map[string]map[string]ScheduledEvent // plugin -> hook -> event
	transients map[string]memoryTransient
}

type memoryTransient struct {
	value   json.RawMessage
	expires time.Time
}

// NewMemoryHost returns an empty MemoryHost.
func NewMemoryHost() *MemoryHost {
	return &MemoryHost{
		options: map[string]any{}, storage: map[string]map[string]string{},
		posts: map[int64]*PostData{}, events: map[string]map[string]ScheduledEvent{},
		transients: map[string]memoryTransient{},
	}
}

func (h *MemoryHost) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// SetOption sets an option directly (test helper).
func (h *MemoryHost) SetOption(name string, value any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.options[name] = value
}

// Option returns an option directly (test helper).
func (h *MemoryHost) Option(name string) (any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.options[name]
	return v, ok
}

// Notices returns the queued notices.
func (h *MemoryHost) Notices() []Notice {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Notice(nil), h.notices...)
}

// AddPost stores a post directly (test helper) and returns its id.
func (h *MemoryHost) AddPost(p PostData) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.ID == 0 {
		h.nextPost++
		p.ID = h.nextPost
	} else if p.ID > h.nextPost {
		h.nextPost = p.ID
	}
	cp := p
	h.posts[p.ID] = &cp
	return p.ID
}

// Events returns the plugin's scheduled events by hook (test helper).
func (h *MemoryHost) Events(plugin string) map[string]ScheduledEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]ScheduledEvent{}
	for k, v := range h.events[plugin] {
		out[k] = v
	}
	return out
}

// GetOption implements Host.
func (h *MemoryHost) GetOption(_ context.Context, _, name string) (any, bool, error) {
	v, ok := h.Option(name)
	return v, ok, nil
}

// UpdateOption implements Host.
func (h *MemoryHost) UpdateOption(_ context.Context, _, name string, value any) error {
	h.SetOption(name, value)
	return nil
}

// HTTPGet implements Host.
func (h *MemoryHost) HTTPGet(ctx context.Context, _, url string) (*HTTPResponse, error) {
	if h.HTTP == nil {
		return nil, ErrHTTPDisabled
	}
	return h.HTTP(ctx, url)
}

// StorageGet implements Host.
func (h *MemoryHost) StorageGet(_ context.Context, plugin, key string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.storage[plugin][key]
	return v, ok, nil
}

// StorageSet implements Host.
func (h *MemoryHost) StorageSet(_ context.Context, plugin, key, value string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.storage[plugin] == nil {
		h.storage[plugin] = map[string]string{}
	}
	h.storage[plugin][key] = value
	return nil
}

// AdminNotice implements Host.
func (h *MemoryHost) AdminNotice(_ context.Context, _ string, n Notice) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notices = append(h.notices, n)
	return nil
}

// GetPost implements Host.
func (h *MemoryHost) GetPost(_ context.Context, _ string, id int64) (*PostData, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.posts[id]
	if !ok {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

// QueryPosts implements Host with a simple in-memory filter.
func (h *MemoryHost) QueryPosts(_ context.Context, _ string, q PostQuery) (*PostList, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	typ, status := q.Type, q.Status
	if typ == "" {
		typ = "post"
	}
	if status == "" {
		status = "publish"
	}
	var all []PostData
	for _, p := range h.posts {
		if p.Type != typ || (status != "any" && p.Status != status) || (q.Author != 0 && p.Author != q.Author) {
			continue
		}
		if q.Search != "" && !strings.Contains(strings.ToLower(p.Title+" "+p.Content), strings.ToLower(q.Search)) {
			continue
		}
		if q.Term != 0 {
			found := false
			for _, ts := range p.Terms {
				for _, t := range ts {
					if t.ID == q.Term {
						found = true
					}
				}
			}
			if !found {
				continue
			}
		}
		all = append(all, *p)
	}
	less := func(a, b *PostData) bool {
		switch q.OrderBy {
		case "title":
			return a.Title < b.Title
		case "id":
			return a.ID < b.ID
		case "modified":
			return a.Modified < b.Modified
		}
		return a.Date < b.Date || (a.Date == b.Date && a.ID < b.ID)
	}
	sort.Slice(all, func(i, j int) bool {
		if strings.EqualFold(q.Order, "ASC") {
			return less(&all[i], &all[j])
		}
		return less(&all[j], &all[i])
	})
	per, page := q.PerPage, q.Page
	if per <= 0 {
		per = 10
	}
	if page <= 0 {
		page = 1
	}
	out := &PostList{Total: len(all), Pages: (len(all) + per - 1) / per, Posts: []PostData{}}
	start := (page - 1) * per
	if start < len(all) {
		end := start + per
		if end > len(all) {
			end = len(all)
		}
		out.Posts = all[start:end]
	}
	return out, nil
}

// InsertPost implements Host.
func (h *MemoryHost) InsertPost(_ context.Context, _ string, in PostInput) (*PostData, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextPost++
	now := h.now().UTC().Format(time.RFC3339)
	p := &PostData{ID: h.nextPost, Type: "post", Status: "draft", Date: now, Modified: now, Terms: map[string][]TermData{}}
	applyPostInput(p, in)
	if p.Slug == "" {
		p.Slug = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p.Title), " ", "-"))
	}
	h.posts[p.ID] = p
	cp := *p
	return &cp, nil
}

// UpdatePost implements Host.
func (h *MemoryHost) UpdatePost(_ context.Context, _ string, id int64, in PostInput) (*PostData, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.posts[id]
	if !ok {
		return nil, errors.New("post not found")
	}
	applyPostInput(p, in)
	p.Modified = h.now().UTC().Format(time.RFC3339)
	cp := *p
	return &cp, nil
}

func applyPostInput(p *PostData, in PostInput) {
	if in.Title != nil {
		p.Title = *in.Title
	}
	if in.Content != nil {
		p.Content = *in.Content
	}
	if in.Excerpt != nil {
		p.Excerpt = *in.Excerpt
	}
	if in.Status != nil {
		p.Status = *in.Status
	}
	if in.Type != nil {
		p.Type = *in.Type
	}
	if in.Slug != nil {
		p.Slug = *in.Slug
	}
	if len(in.Meta) > 0 {
		if p.Meta == nil {
			p.Meta = map[string]any{}
		}
		for k, v := range in.Meta {
			if v == nil {
				delete(p.Meta, k)
			} else {
				p.Meta[k] = v
			}
		}
	}
}

// CurrentUser implements Host.
func (h *MemoryHost) CurrentUser(context.Context, string) (*UserInfo, error) {
	if h.User == nil {
		return nil, nil
	}
	cp := *h.User
	return &cp, nil
}

// ScheduleEvent implements Host.
func (h *MemoryHost) ScheduleEvent(_ context.Context, plugin string, ev ScheduledEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.events[plugin] == nil {
		h.events[plugin] = map[string]ScheduledEvent{}
	}
	h.events[plugin][ev.Hook] = ev
	return nil
}

// UnscheduleEvent implements Host.
func (h *MemoryHost) UnscheduleEvent(_ context.Context, plugin, hook string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.events[plugin], hook)
	return nil
}

// TransientGet implements Host.
func (h *MemoryHost) TransientGet(_ context.Context, plugin, key string) (json.RawMessage, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.transients[plugin+"\x00"+key]
	if !ok || (!t.expires.IsZero() && !h.now().Before(t.expires)) {
		delete(h.transients, plugin+"\x00"+key)
		return nil, false, nil
	}
	return t.value, true, nil
}

// TransientSet implements Host.
func (h *MemoryHost) TransientSet(_ context.Context, plugin, key string, value json.RawMessage, ttl time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := memoryTransient{value: append(json.RawMessage(nil), value...)}
	if ttl > 0 {
		t.expires = h.now().Add(ttl)
	}
	h.transients[plugin+"\x00"+key] = t
	return nil
}

// TransientDelete implements Host.
func (h *MemoryHost) TransientDelete(_ context.Context, plugin, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.transients, plugin+"\x00"+key)
	return nil
}
