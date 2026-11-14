package cache

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotCacheable is returned by [Store.Set] for entries that must never be stored,
// such as responses carrying Set-Cookie.
var ErrNotCacheable = errors.New("cache: response not cacheable")

// ErrPurged is returned by [Store.SetSince] when a purge that covers the entry ran after the
// generation the entry was rendered at: storing it would bring back the content just purged.
var ErrPurged = errors.New("cache: purged while rendering")

// Entry is one cached response.
type Entry struct {
	Key     string      // canonical cache key (see [Cache.Key])
	Status  int         // HTTP status code
	Header  http.Header // filtered response headers (no Set-Cookie, hop-by-hop or length)
	Body    []byte      // identity-encoded body
	Gzip    []byte      // gzip-compressed body, computed once at store time
	ETag    string      // strong ETag of Body, quoted
	Created time.Time
	Expires time.Time
	Tags    []string
}

func (e *Entry) clone() *Entry {
	copyOfEntry := *e
	copyOfEntry.Header = e.Header.Clone()
	copyOfEntry.Body = bytes.Clone(e.Body)
	copyOfEntry.Gzip = bytes.Clone(e.Gzip)
	copyOfEntry.Tags = slices.Clone(e.Tags)
	return &copyOfEntry
}

// Fresh reports whether the entry has not expired at now.
func (e *Entry) Fresh(now time.Time) bool { return now.Before(e.Expires) }

func (e *Entry) size() int64 {
	n := int64(len(e.Key) + len(e.Body) + len(e.Gzip) + len(e.ETag) + 128)
	for k, vs := range e.Header {
		n += int64(len(k))
		for _, v := range vs {
			n += int64(len(v))
		}
	}
	for _, t := range e.Tags {
		n += int64(len(t))
	}
	return n
}

// item is the store's record of a key. A record whose entry is nil lives only on
// disk; its body is loaded on the next Get.
type item struct {
	key      string
	base     string // host+path, for PurgeURL
	tags     []string
	expires  time.Time
	entry    *Entry
	elem     *list.Element // position in the LRU list while resident in memory
	diskElem *list.Element // position in the disk-only list while only on disk
	bytes    int64
	file     int64 // approximate size of its disk copy, counted in stubBytes while only on disk
}

// Stats is a snapshot of store counters.
type Stats struct {
	Entries     int            // entries resident in memory
	DiskEntries int            // entries known only on disk (body not loaded)
	Bytes       int64          // memory used by resident entries
	Hits        int64          // Get calls that returned a fresh entry
	Misses      int64          // Get calls that did not
	HitRatio    float64        // Hits / (Hits + Misses), 0 when no lookups yet
	Tags        map[string]int // number of entries (memory and disk) per tag
}

// Store is a concurrency-safe, size-bounded LRU of cached responses with a tag index
// and optional disk persistence.
type Store struct {
	maxEntries     int
	maxBytes       int64
	maxDiskEntries int   // bound on records that live only on disk (files and index entries)
	maxDiskBytes   int64 // bound on the bytes of those records' files
	disk           *diskStore
	now            func() time.Time
	log            *slog.Logger

	mu        sync.Mutex
	items     map[string]*item
	lru       *list.List // front = most recently used; values are *item
	stubs     *list.List // disk-only records, front = most recently unloaded; values are *item
	tags      map[string]map[string]struct{}
	bases     map[string]map[string]struct{}
	bytes     int64
	stubBytes int64 // file bytes of the disk-only records
	searches  int   // entries (memory and disk) of search result pages, bounded by maxSearchEntries
	hits      atomic.Int64
	misses    atomic.Int64

	// Purge generations: gen counts purges; allGen, tagGen, baseGen (host+path) and pathGen (a
	// path on every host) remember the generation of the last purge of each scope, so SetSince
	// can refuse a render that a purge overtook.
	gen     uint64
	allGen  uint64
	tagGen  map[string]uint64
	baseGen map[string]uint64
	pathGen map[string]uint64
}

// maxGenScopes bounds the purge-generation maps; past it they collapse into allGen (which can
// only make SetSince refuse more, never less).
const maxGenScopes = 10000

// StoreOptions configures [NewStore].
type StoreOptions struct {
	MaxEntries int   // default DefaultMaxEntries
	MaxBytes   int64 // default DefaultMaxBytes
	// MaxDiskEntries bounds the entries kept only on disk (default 10 × MaxEntries): past it the oldest
	// files are deleted, so arbitrary query values (?s=, ?page=) cannot fill the disk and the index.
	MaxDiskEntries int
	// MaxDiskBytes bounds the file bytes of those entries (default 4 × MaxBytes): a count alone lets
	// large pages (up to MaxEntrySize each, plus their gzip copy) fill the disk.
	MaxDiskBytes int64
	DiskDir      string           // optional persistence directory
	Now          func() time.Time // clock, default time.Now
	Logger       *slog.Logger     // default slog.Default()
}

// NewStore creates a store. With a DiskDir, the directory is created if needed and the
// metadata of persisted entries (key, tags, expiry) is indexed; bodies load lazily.
func NewStore(o StoreOptions) (*Store, error) {
	s := &Store{
		maxEntries:     o.MaxEntries,
		maxBytes:       o.MaxBytes,
		maxDiskEntries: o.MaxDiskEntries,
		maxDiskBytes:   o.MaxDiskBytes,
		now:            o.Now,
		log:            o.Logger,
		items:          make(map[string]*item),
		lru:            list.New(),
		stubs:          list.New(),
		tags:           make(map[string]map[string]struct{}),
		bases:          make(map[string]map[string]struct{}),
	}
	if s.maxEntries <= 0 {
		s.maxEntries = DefaultMaxEntries
	}
	if s.maxBytes <= 0 {
		s.maxBytes = DefaultMaxBytes
	}
	if s.maxDiskEntries <= 0 {
		if s.maxEntries > math.MaxInt/10 {
			s.maxDiskEntries = math.MaxInt
		} else {
			s.maxDiskEntries = 10 * s.maxEntries
		}
	}
	if s.maxDiskBytes <= 0 {
		if s.maxBytes > math.MaxInt64/4 {
			s.maxDiskBytes = math.MaxInt64
		} else {
			s.maxDiskBytes = 4 * s.maxBytes
		}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if o.DiskDir != "" {
		d, err := openDisk(o.DiskDir)
		if err != nil {
			return nil, err
		}
		s.disk = d
		now := s.now()
		err = d.scan(func(m diskMeta, size int64) {
			if !now.Before(m.Expires) {
				d.remove(m.Key)
				return
			}
			it := &item{key: m.Key, base: keyBase(m.Key), tags: m.Tags, expires: m.Expires, file: size}
			s.items[m.Key] = it
			s.index(it)
			s.pushStubLocked(it)
		})
		if err != nil {
			return nil, err
		}
		s.trimDiskLocked()
	}
	return s, nil
}

// NewEntry builds an entry for key from a response, filtering headers and computing
// the gzip body and ETag. It returns ErrNotCacheable when the header has Set-Cookie.
func NewEntry(key string, status int, header http.Header, body []byte, tags []string, created time.Time, ttl time.Duration) (*Entry, error) {
	if len(header.Values("Set-Cookie")) > 0 {
		return nil, ErrNotCacheable
	}
	e := &Entry{
		Key:     key,
		Status:  status,
		Header:  filterHeader(header),
		Body:    bytes.Clone(body),
		Created: created,
		Expires: created.Add(ttl),
		Tags:    normalizeTags(tags),
	}
	sum := sha256.Sum256(body)
	e.ETag = `"` + hex.EncodeToString(sum[:16]) + `"`
	if len(body) >= 256 {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
		zw.Write(body)
		if zw.Close() == nil && buf.Len() < len(body) {
			e.Gzip = buf.Bytes()
		}
	}
	return e, nil
}

// unstoredHeaders are never kept in an entry: they are per-response, per-connection
// or recomputed when serving.
var unstoredHeaders = []string{
	"Set-Cookie", "Content-Length", "Content-Encoding", "Transfer-Encoding", "Connection",
	"Keep-Alive", "Date", "Age", "Etag", "Trailer", "Upgrade", "Proxy-Connection", "Te",
	"Proxy-Authenticate", "Proxy-Authorization",
	HeaderTags, HeaderTTL, HeaderControl, HeaderStatus,
}

// requestIDHeader is server.HeaderRequestID (the cache package cannot import server). It is set per request
// by the outer ops handler before the cache runs, so it is neither stored nor replayed on a hit; unlike
// unstoredHeaders it is not deleted from the live response either.
const requestIDHeader = "X-Request-Id"

func filterHeader(h http.Header) http.Header {
	out := h.Clone()
	for _, value := range h.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			out.Del(strings.TrimSpace(name))
		}
	}
	for _, k := range unstoredHeaders {
		out.Del(k)
	}
	out.Del(requestIDHeader)
	return out
}

func normalizeTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Get returns an independent copy of the fresh entry for key, loading it from disk if needed,
// and counts a hit or a miss. Expired entries are removed.
func (s *Store) Get(key string) (*Entry, bool) {
	e, ok := s.getShared(key)
	if !ok {
		return nil, false
	}
	return e.clone(), true
}

// getShared is for cache internals that only read immutable stored entries.
func (s *Store) getShared(key string) (*Entry, bool) {
	e, ok := s.get(key)
	if ok {
		s.hits.Add(1)
	} else {
		s.misses.Add(1)
	}
	return e, ok
}

func (s *Store) get(key string) (*Entry, bool) {
	s.mu.Lock()
	it, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return nil, false
	}
	if !s.now().Before(it.expires) {
		s.removeLocked(it, true)
		s.mu.Unlock()
		return nil, false
	}
	if it.entry != nil {
		s.lru.MoveToFront(it.elem)
		e := it.entry
		s.mu.Unlock()
		return e, true
	}
	s.mu.Unlock()

	// Disk-only: load outside the lock, then re-check that the record is unchanged.
	e, err := s.disk.load(key)
	if err != nil {
		s.log.Warn("wpgo-cache: loading persisted entry", "key", key, "err", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.items[key]
	if !ok || cur != it {
		return nil, false
	}
	if !s.now().Before(it.expires) {
		s.removeLocked(it, true)
		return nil, false
	}
	if e == nil {
		s.removeLocked(it, true)
		return nil, false
	}
	if it.entry == nil {
		s.dropStubLocked(it)
		it.entry = e
		it.bytes = e.size()
		it.elem = s.lru.PushFront(it)
		s.bytes += it.bytes
		s.evictLocked(it)
	}
	return it.entry, true
}

// Set stores a copy of e, replacing any entry with the same key, persisting it to disk when
// configured, and evicting least recently used entries to respect the limits.
// Entries that alone exceed MaxBytes are not stored.
func (s *Store) Set(e *Entry) error { return s.set(e, false, 0) }

// Generation is the current purge generation: take it before rendering a page and pass it to
// [Store.SetSince].
func (s *Store) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// SetSince stores e like [Store.Set] unless a purge covering it (everything, one of its tags,
// its URL) ran after generation since; then it returns [ErrPurged] and stores nothing.
func (s *Store) SetSince(e *Entry, since uint64) error { return s.set(e, true, since) }

func (s *Store) set(e *Entry, check bool, since uint64) error {
	if e == nil || e.Key == "" {
		return errors.New("cache: entry without key")
	}
	e = e.clone()
	if len(e.Header.Values("Set-Cookie")) > 0 {
		return ErrNotCacheable
	}
	size := e.size()
	if size > s.maxBytes {
		return ErrNotCacheable
	}
	s.mu.Lock()
	if check && s.purgedSinceLocked(e, since) {
		s.mu.Unlock()
		return ErrPurged
	}
	old, replacing := s.items[e.Key]
	if !replacing && s.searches >= maxSearchEntries && isSearchKey(e.Key) {
		s.mu.Unlock()
		return ErrNotCacheable
	}
	if replacing {
		s.removeLocked(old, false)
	}
	it := &item{key: e.Key, base: keyBase(e.Key), tags: e.Tags, expires: e.Expires, entry: e, bytes: size, file: size}
	it.elem = s.lru.PushFront(it)
	s.items[e.Key] = it
	s.bytes += size
	s.index(it)
	s.evictLocked(it)
	s.mu.Unlock()

	if s.disk != nil {
		if err := s.disk.save(e); err != nil {
			s.log.Warn("wpgo-cache: persisting entry", "key", e.Key, "err", err)
		}
		// A purge that ran while the file was being written must not leave it behind.
		s.mu.Lock()
		if _, ok := s.items[e.Key]; !ok {
			s.disk.remove(e.Key)
		}
		s.mu.Unlock()
	}
	return nil
}

// evictLocked drops least recently used resident entries until limits hold, never
// evicting keep. With disk persistence evicted entries stay on disk as stubs.
func (s *Store) evictLocked(keep *item) {
	for s.lru.Len() > s.maxEntries || s.bytes > s.maxBytes {
		el := s.lru.Back()
		if el == nil {
			return
		}
		it := el.Value.(*item)
		if it == keep {
			if s.lru.Len() == 1 {
				return
			}
			s.lru.MoveToFront(el)
			continue
		}
		if s.disk != nil {
			s.unloadLocked(it)
			s.pushStubLocked(it)
		} else {
			s.removeLocked(it, false)
		}
	}
	s.trimDiskLocked()
}

// trimDiskLocked deletes the oldest disk-only records (file and index) past maxDiskEntries or
// maxDiskBytes.
func (s *Store) trimDiskLocked() {
	for s.stubs.Len() > 0 && (s.stubs.Len() > s.maxDiskEntries || s.stubBytes > s.maxDiskBytes) {
		s.removeLocked(s.stubs.Back().Value.(*item), true)
	}
}

func (s *Store) pushStubLocked(it *item) {
	it.diskElem = s.stubs.PushFront(it)
	s.stubBytes += it.file
}

func (s *Store) dropStubLocked(it *item) {
	if it.diskElem != nil {
		s.stubs.Remove(it.diskElem)
		it.diskElem = nil
		s.stubBytes -= it.file
	}
}

// unloadLocked frees an entry's memory but keeps its disk-backed record.
func (s *Store) unloadLocked(it *item) {
	if it.elem != nil {
		s.lru.Remove(it.elem)
		it.elem = nil
	}
	s.bytes -= it.bytes
	it.bytes = 0
	it.entry = nil
}

// removeLocked forgets an item entirely; deleteFile also removes its disk copy.
func (s *Store) removeLocked(it *item, deleteFile bool) {
	s.unloadLocked(it)
	s.dropStubLocked(it)
	delete(s.items, it.key)
	if isSearchKey(it.key) {
		s.searches--
	}
	for _, t := range it.tags {
		if set := s.tags[t]; set != nil {
			delete(set, it.key)
			if len(set) == 0 {
				delete(s.tags, t)
			}
		}
	}
	if set := s.bases[it.base]; set != nil {
		delete(set, it.key)
		if len(set) == 0 {
			delete(s.bases, it.base)
		}
	}
	if deleteFile && s.disk != nil {
		s.disk.remove(it.key)
	}
}

// purgedSinceLocked reports whether a purge after generation since covers e.
func (s *Store) purgedSinceLocked(e *Entry, since uint64) bool {
	if s.gen == since {
		return false
	}
	if s.allGen > since {
		return true
	}
	base := keyBase(e.Key)
	if s.baseGen[base] > since {
		return true
	}
	if i := strings.IndexByte(base, '/'); i >= 0 && s.pathGen[base[i:]] > since {
		return true
	}
	for _, t := range e.Tags {
		if s.tagGen[t] > since {
			return true
		}
	}
	return false
}

// bumpLocked starts a new purge generation and returns it.
func (s *Store) bumpLocked() uint64 {
	s.gen++
	if len(s.tagGen)+len(s.baseGen)+len(s.pathGen) > maxGenScopes {
		s.allGen, s.tagGen, s.baseGen, s.pathGen = s.gen, nil, nil, nil
	}
	return s.gen
}

func markGen(m *map[string]uint64, k string, g uint64) {
	if *m == nil {
		*m = make(map[string]uint64)
	}
	(*m)[k] = g
}

func (s *Store) index(it *item) {
	if isSearchKey(it.key) {
		s.searches++
	}
	for _, t := range it.tags {
		set := s.tags[t]
		if set == nil {
			set = make(map[string]struct{})
			s.tags[t] = set
		}
		set[it.key] = struct{}{}
	}
	set := s.bases[it.base]
	if set == nil {
		set = make(map[string]struct{})
		s.bases[it.base] = set
	}
	set[it.key] = struct{}{}
}

// Delete removes one key. It reports whether the key was present.
func (s *Store) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	markGen(&s.baseGen, keyBase(key), s.bumpLocked())
	it, ok := s.items[key]
	if ok {
		s.removeLocked(it, true)
	}
	return ok
}

// PurgeAll removes every entry, in memory and on disk.
func (s *Store) PurgeAll() int {
	s.mu.Lock()
	s.allGen = s.bumpLocked()
	s.tagGen, s.baseGen, s.pathGen = nil, nil, nil
	n := len(s.items)
	s.items = make(map[string]*item)
	s.lru.Init()
	s.stubs.Init()
	s.stubBytes = 0
	s.tags = make(map[string]map[string]struct{})
	s.bases = make(map[string]map[string]struct{})
	s.bytes = 0
	s.mu.Unlock()
	if s.disk != nil {
		s.disk.clear()
	}
	return n
}

// PurgeTags removes every entry carrying at least one of tags and returns how many
// entries were removed.
func (s *Store) PurgeTags(tags ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.bumpLocked()
	for _, t := range tags {
		markGen(&s.tagGen, strings.TrimSpace(t), g)
	}
	n := 0
	for _, t := range tags {
		for key := range s.tags[strings.TrimSpace(t)] {
			if it, ok := s.items[key]; ok {
				s.removeLocked(it, true)
				n++
			}
		}
	}
	return n
}

// PurgeURL removes every cached variant of a URL's path: all vary buckets and all
// cached query strings (search, pagination). The scheme is ignored; a URL without a
// host ("/hello-world/") purges that path on every host.
func (s *Store) PurgeURL(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	host := strings.ToLower(u.Host)
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.bumpLocked(); host != "" {
		markGen(&s.baseGen, host+p, g)
	} else {
		markGen(&s.pathGen, p, g)
	}
	var bases []string
	if host != "" {
		bases = []string{host + p}
	} else {
		for b := range s.bases {
			if i := strings.IndexByte(b, '/'); i >= 0 && b[i:] == p {
				bases = append(bases, b)
			}
		}
	}
	n := 0
	for _, b := range bases {
		for key := range s.bases[b] {
			if it, ok := s.items[key]; ok {
				s.removeLocked(it, true)
				n++
			}
		}
	}
	return n
}

// PurgeExpired removes entries whose TTL has elapsed and returns how many were removed.
func (s *Store) PurgeExpired() int {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, it := range s.items {
		if !now.Before(it.expires) {
			s.removeLocked(it, true)
			n++
		}
	}
	return n
}

// Stats returns a snapshot of the store's counters.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	st := Stats{
		Entries:     s.lru.Len(),
		DiskEntries: len(s.items) - s.lru.Len(),
		Bytes:       s.bytes,
		Tags:        make(map[string]int, len(s.tags)),
	}
	for t, set := range s.tags {
		st.Tags[t] = len(set)
	}
	s.mu.Unlock()
	st.Hits, st.Misses = s.hits.Load(), s.misses.Load()
	if total := st.Hits + st.Misses; total > 0 {
		st.HitRatio = float64(st.Hits) / float64(total)
	}
	return st
}

// isSearchKey reports whether a canonical key ("scheme://host/path?query#vary") is a search result
// page: its query (sorted and encoded by keyQuery) carries the s parameter.
func isSearchKey(key string) bool {
	i := strings.IndexByte(key, '?')
	if i < 0 {
		return false
	}
	q := key[i+1:]
	if j := strings.IndexByte(q, '#'); j >= 0 {
		q = q[:j]
	}
	return strings.HasPrefix(q, "s=") || strings.Contains(q, "&s=")
}

// keyBase extracts host+path from a canonical key ("https://host/path?q#vary").
func keyBase(key string) string {
	k := key
	if i := strings.Index(k, "://"); i >= 0 {
		k = k[i+3:]
	}
	if i := strings.IndexAny(k, "?#"); i >= 0 {
		k = k[:i]
	}
	return k
}
