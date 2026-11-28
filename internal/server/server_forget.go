package server

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Process-wide state keyed by *Server. Several features keep per-Server state in package-level
// maps (one Server per process in production, but a theme-preview sandbox is a second Server in
// the same process, and tests build many). Each entry pins its whole Server (templates, gin
// engine, catalogs, settings), so a Server that goes away must be forgotten: forgetProcessState
// deletes it from every such map. TestForgetProcessStateCoversEveryServerMap type-checks this
// package and fails when a package-level variable that can hold a *Server is not registered here.

// processStateForget clears one package-level variable's entry for a Server.
type processStateForget struct {
	name   string // the package-level variable
	forget func(s *Server)
}

// syncMapForget forgets s in a sync.Map keyed by *Server.
func syncMapForget(name string, m *sync.Map) processStateForget {
	return processStateForget{name, func(s *Server) { m.Delete(s) }}
}

var processStateForgets = []processStateForget{
	syncMapForget("acfgoBlockCaches", &acfgoBlockCaches),
	syncMapForget("acfgoCaches", &acfgoCaches),
	syncMapForget("fwLimiters", &fwLimiters),
	syncMapForget("gooGiftCardThrottles", &gooGiftCardThrottles),
	syncMapForget("gooGuestCheckoutThrottles", &gooGuestCheckoutThrottles),
	syncMapForget("gocdnRuntimes", &gocdnRuntimes),
	syncMapForget("hgBrokers", &hgBrokers),
	syncMapForget("hgEngines", &hgEngines),
	syncMapForget("hgRateBuckets", &hgRateBuckets),
	syncMapForget("hgRegStates", &hgRegStates),
	syncMapForget("pllCapturers", &pllCapturers),
	syncMapForget("pllMediaLangSet", &pllMediaLangSet),
	syncMapForget("pllOptionMigrated", &pllOptionMigrated),
	syncMapForget("pllStates", &pllStates),
	syncMapForget("seogoState", &seogoState),
	syncMapForget("wbFormLimiters", &wbFormLimiters),
	syncMapForget("wbTemplateStates", &wbTemplateStates),
	{"forumSiteCache", func(s *Server) {
		forumSiteCache.Lock()
		delete(forumSiteCache.sets, s)
		forumSiteCache.Unlock()
	}},
	{"forumTplCache", func(s *Server) {
		forumTplCache.Lock()
		delete(forumTplCache.sets, s)
		forumTplCache.Unlock()
	}},
	{"gooTplCache", func(s *Server) {
		gooTplCache.Lock()
		delete(gooTplCache.sets, s)
		gooTplCache.Unlock()
	}},
	{"pllFrontTables", func(s *Server) {
		pllStringsMu.Lock()
		delete(pllFrontTables, s)
		pllStringsMu.Unlock()
	}},
	{"pllStringsCache", func(s *Server) {
		pllStringsMu.Lock()
		delete(pllStringsCache, s)
		pllStringsMu.Unlock()
	}},
	{"pllJobs", func(s *Server) {
		pllJobs.Lock()
		delete(pllJobs.m, s)
		pllJobs.Unlock()
	}},
	{"seogoIndexNowHistory", func(s *Server) {
		seogoIndexNowHistory.Lock()
		delete(seogoIndexNowHistory.m, s)
		seogoIndexNowHistory.Unlock()
	}},
	{"seogoLinkCache", func(s *Server) {
		seogoLinkCache.Lock()
		delete(seogoLinkCache.m, s)
		seogoLinkCache.Unlock()
	}},
	{"seogoRedirects", func(s *Server) {
		seogoRedirects.Lock()
		delete(seogoRedirects.m, s)
		seogoRedirects.Unlock()
	}},
	// Keyed by "<%p of the Server>/<slug>": no pointer is kept, but the entries would stay, and a
	// later Server at the same address could read them.
	{"wbKitRootLiveCache", func(s *Server) {
		prefix := fmt.Sprintf("%p/", s)
		wbKitRootLiveCache.Range(func(k, _ any) bool {
			if key, ok := k.(string); ok && strings.HasPrefix(key, prefix) {
				wbKitRootLiveCache.Delete(k)
			}
			return true
		})
	}},
	// Deliveries in flight hold their Server until they finish (the last retry runs about a minute
	// after the first attempt); a channel cannot be cleared per Server, and a sandbox runs no plugin
	// that sends webhooks.
	{"gooWebhookQueue", func(*Server) {}},
}

// processStateForgetUnscanned are registered variables the type scan cannot see (keys that only
// encode a Server's address).
var processStateForgetUnscanned = map[string]bool{"wbKitRootLiveCache": true}

// processStateResweep is how long after a Server is closed forgetProcessState runs again: timers a
// request started (the Multi Language string capture flushes pllCaptureDelay later) can look the
// Server up once more and put an entry back.
const processStateResweep = 10 * time.Second

// forgetProcessState removes s from every package-level map keyed by *Server. Call it only once s
// serves no more requests (a closed theme-preview sandbox); the site's own Server is never forgotten.
func (s *Server) forgetProcessState() {
	for _, f := range processStateForgets {
		f.forget(s)
	}
}
