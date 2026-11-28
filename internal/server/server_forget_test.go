package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"wpgo/internal/i18n"
	"wpgo/web"
)

// processStateHas reports, per registered variable, whether it holds an entry for s.
var processStateHas = map[string]func(s *Server) bool{
	"acfgoBlockCaches":          syncMapHas(&acfgoBlockCaches),
	"acfgoCaches":               syncMapHas(&acfgoCaches),
	"fwLimiters":                syncMapHas(&fwLimiters),
	"gooGiftCardThrottles":      syncMapHas(&gooGiftCardThrottles),
	"gooGuestCheckoutThrottles": syncMapHas(&gooGuestCheckoutThrottles),
	"gocdnRuntimes":             syncMapHas(&gocdnRuntimes),
	"hgBrokers":                 syncMapHas(&hgBrokers),
	"hgEngines":                 syncMapHas(&hgEngines),
	"hgRateBuckets":             syncMapHas(&hgRateBuckets),
	"hgRegStates":               syncMapHas(&hgRegStates),
	"pllCapturers":              syncMapHas(&pllCapturers),
	"pllMediaLangSet":           syncMapHas(&pllMediaLangSet),
	"pllOptionMigrated":         syncMapHas(&pllOptionMigrated),
	"pllStates":                 syncMapHas(&pllStates),
	"seogoState":                syncMapHas(&seogoState),
	"wbFormLimiters":            syncMapHas(&wbFormLimiters),
	"wbTemplateStates":          syncMapHas(&wbTemplateStates),
	"forumSiteCache": func(s *Server) bool {
		forumSiteCache.Lock()
		defer forumSiteCache.Unlock()
		_, ok := forumSiteCache.sets[s]
		return ok
	},
	"forumTplCache": func(s *Server) bool {
		forumTplCache.Lock()
		defer forumTplCache.Unlock()
		_, ok := forumTplCache.sets[s]
		return ok
	},
	"gooTplCache": func(s *Server) bool {
		gooTplCache.Lock()
		defer gooTplCache.Unlock()
		_, ok := gooTplCache.sets[s]
		return ok
	},
	"pllFrontTables": func(s *Server) bool {
		pllStringsMu.Lock()
		defer pllStringsMu.Unlock()
		_, ok := pllFrontTables[s]
		return ok
	},
	"pllStringsCache": func(s *Server) bool {
		pllStringsMu.Lock()
		defer pllStringsMu.Unlock()
		_, ok := pllStringsCache[s]
		return ok
	},
	"pllJobs": func(s *Server) bool {
		pllJobs.Lock()
		defer pllJobs.Unlock()
		_, ok := pllJobs.m[s]
		return ok
	},
	"seogoIndexNowHistory": func(s *Server) bool {
		seogoIndexNowHistory.Lock()
		defer seogoIndexNowHistory.Unlock()
		_, ok := seogoIndexNowHistory.m[s]
		return ok
	},
	"seogoLinkCache": func(s *Server) bool {
		seogoLinkCache.Lock()
		defer seogoLinkCache.Unlock()
		_, ok := seogoLinkCache.m[s]
		return ok
	},
	"seogoRedirects": func(s *Server) bool {
		seogoRedirects.Lock()
		defer seogoRedirects.Unlock()
		_, ok := seogoRedirects.m[s]
		return ok
	},
	"wbKitRootLiveCache": func(s *Server) bool {
		found := false
		prefix := fmt.Sprintf("%p/", s)
		wbKitRootLiveCache.Range(func(k, _ any) bool {
			found = strings.HasPrefix(k.(string), prefix)
			return !found
		})
		return found
	},
}

// processStateNoEntry are registered variables a test cannot fill for a Server.
var processStateNoEntry = map[string]bool{"gooWebhookQueue": true}

func syncMapHas(m *sync.Map) func(*Server) bool {
	return func(s *Server) bool {
		_, ok := m.Load(s)
		return ok
	}
}

// fillProcessState gives s an entry in every registered variable: through the features' own
// accessors where they have one, directly otherwise (with the types the features expect).
func fillProcessState(t *testing.T, s *Server) {
	t.Helper()
	s.fwLimiter()
	s.gocdnRT()
	s.pllState()
	s.pllCapture()
	s.acfgoCache()
	s.hgBroker()
	s.hgRegState()
	s.hgEngine()
	s.seogoRT()
	s.wbFormLimiter()
	s.gooGuestCheckoutThrottle()
	s.gooGiftCardThrottle()
	s.wbTemplateState()
	s.gooTemplates()
	s.forumTemplates()
	acfgoBlockCaches.LoadOrStore(s, &acfgoBlockCache{})
	hgRateBuckets.LoadOrStore(s, &hgRateState{m: map[string]*hgBucket{}})
	pllMediaLangSet.Store(s, &pllMediaLangCache{at: time.Now(), langs: map[string]bool{}})
	pllOptionMigrated.Store(s, true)
	wbKitRootLiveCache.Store(fmt.Sprintf("%p/%s", s, "kit"), wbKitRootLiveEntry{at: time.Now()})
	forumSiteCache.Lock()
	if forumSiteCache.sets == nil {
		forumSiteCache.sets = map[*Server]*forumSiteSet{}
	}
	forumSiteCache.sets[s] = &forumSiteSet{}
	forumSiteCache.Unlock()
	pllStringsMu.Lock()
	pllFrontTables[s] = map[string]*pllFrontTable{}
	pllStringsCache[s] = map[string]map[string]string{}
	pllStringsMu.Unlock()
	pllJobs.Lock()
	pllJobs.m[s] = &pllJob{}
	pllJobs.Unlock()
	seogoIndexNowHistory.Lock()
	seogoIndexNowHistory.m[s] = nil
	seogoIndexNowHistory.Unlock()
	seogoLinkCache.Lock()
	seogoLinkCache.m[s] = seogoLinkCacheEntry{}
	seogoLinkCache.Unlock()
	seogoRedirects.Lock()
	seogoRedirects.m[s] = &seogoRedirectCache{}
	seogoRedirects.Unlock()
}

// heldBy lists the registered variables that hold an entry for s.
func heldBy(s *Server) []string {
	var out []string
	for _, f := range processStateForgets {
		if has := processStateHas[f.name]; has != nil && has(s) {
			out = append(out, f.name)
		}
	}
	return out
}

func TestForgetProcessStateChecksEveryVariable(t *testing.T) {
	for _, f := range processStateForgets {
		if processStateHas[f.name] == nil && !processStateNoEntry[f.name] {
			t.Errorf("%s has no check in processStateHas (server_forget_test.go)", f.name)
		}
	}
}

// TestThemePreviewSandboxForgotten: a closed theme-preview sandbox leaves nothing of itself in the
// process-wide maps, and the site's own entries stay.
func TestThemePreviewSandboxForgotten(t *testing.T) {
	t.Setenv("PURSAE_THEMES_DIR", t.TempDir())
	lib := libraryCopy(t)
	ts, c, srv := testServerBuilderOff(t)
	srv.themeLibrary = os.DirFS(lib)
	t.Cleanup(srv.closePreviews)
	base := ts.URL
	if code, body, _ := post(t, c, base+"/wp-admin/install?step=2", url.Values{
		"weblog_title": {"Main Site"}, "user_name": {"admin"}, "admin_password": {"correct horse"}, "admin_email": {"admin@example.test"},
	}); code != http.StatusOK {
		t.Fatalf("install: %d %s", code, body)
	}
	if code, _, _ := post(t, c, base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"correct horse"}}); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	if code, body, _ := get(t, c, base+"/wp-admin/theme-preview?theme=wpgo-fixture"); code != http.StatusOK {
		t.Fatalf("preview screen: %d %s", code, body)
	}
	waitReady(t, c, base, "wpgo-fixture")
	prefix := base + "/wp-admin/theme-preview/wpgo-fixture"
	for _, p := range []string{"/", "/blog/", "/?s=coffee", "/feed/", "/wp-json/", "/no-such-page/"} {
		if code, body, _ := get(t, c, prefix+p); code >= 500 {
			t.Fatalf("sandbox %s: %d %s", p, code, body)
		}
	}
	b := srv.previews.get("wpgo-fixture")
	b.mu.RLock()
	sb, pool := b.srv, b.pool
	b.mu.RUnlock()
	if sb == nil || sb == srv {
		t.Fatal("no sandbox server")
	}
	fillProcessState(t, sb)
	srv.fwLimiter()
	srv.seogoRT()
	srv.gooTemplates()
	held := heldBy(sb)
	if len(held) != len(processStateHas) {
		t.Fatalf("the sandbox is in %d of %d variables: %v", len(held), len(processStateHas), held)
	}

	srv.closePreviews()
	if left := heldBy(sb); len(left) != 0 {
		t.Fatalf("closed sandbox still held by %v", left)
	}
	if pool.Stat().TotalConns() != 0 {
		t.Fatal("the sandbox pool is open")
	}
	for _, name := range []string{"fwLimiters", "seogoState", "gooTplCache"} {
		if !processStateHas[name](srv) {
			t.Fatalf("the site's own entry in %s was removed", name)
		}
	}
	if err := srv.store.Pool.Ping(context.Background()); err != nil {
		t.Fatalf("the site's pool: %v", err)
	}
	if code, _, _ := get(t, c, base+"/"); code != http.StatusOK {
		t.Fatalf("site home after the sandbox closed: %d", code)
	}

	// A later rebuild (same key) reopens the kept sandbox: a new Server, forgotten again on close.
	if code, body, _ := get(t, c, base+"/wp-admin/theme-preview?theme=wpgo-fixture"); code != http.StatusOK {
		t.Fatalf("preview screen again: %d %s", code, body)
	}
	waitReady(t, c, base, "wpgo-fixture")
	if code, _, _ := get(t, c, prefix+"/"); code != http.StatusOK {
		t.Fatalf("reopened sandbox home: %d", code)
	}
	b2 := srv.previews.get("wpgo-fixture")
	b2.mu.RLock()
	sb2 := b2.srv
	b2.mu.RUnlock()
	if sb2 == nil || sb2 == sb {
		t.Fatal("the reopened sandbox has no new server")
	}
	fillProcessState(t, sb2)
	srv.closePreviews()
	if left := heldBy(sb2); len(left) != 0 {
		t.Fatalf("closed reopened sandbox still held by %v", left)
	}
}

// English admin requests get a fresh empty catalog each time; the dictionary script cache must not
// keep one entry per request.
func TestAdminL10nDoesNotCacheEmptyCatalogs(t *testing.T) {
	s := &Server{web: web.FS("")}
	before := 0
	adminL10nCache.Range(func(any, any) bool { before++; return true })
	for i := 0; i < 5; i++ {
		if l := s.adminL10nFor(&i18n.Catalog{Locale: "en_US"}); l.js != "" {
			t.Fatalf("empty catalog produced a dictionary: %q", l.js)
		}
	}
	after := 0
	adminL10nCache.Range(func(any, any) bool { after++; return true })
	if after != before {
		t.Fatalf("adminL10nCache grew from %d to %d entries for empty catalogs", before, after)
	}
}
