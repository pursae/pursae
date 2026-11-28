package plugins

import (
	"strings"
	"testing"
)

// The runtime caches wpgo_alloc per instance; a guest whose wpgo_alloc calls a host import needs
// wpgo_alloc again for that call's response while the cached function is still running, and must
// get a fresh function rather than re-entering the running one.
func TestNestedAllocGetsFreshFunction(t *testing.T) {
	rt := newTestRuntime(t, Config{})
	p := loadPlugin(t, rt, "internal/plugins/testdata/reentrant", NewMemoryHost())
	var outer, nested int
	for _, e := range p.Logs() {
		switch {
		case strings.Contains(e.Message, "outer"):
			outer++
		case strings.Contains(e.Message, "nested"):
			nested++
		}
	}
	if outer != 3 || nested < 3 {
		dumpLogs(t, p)
		t.Fatalf("outer %d nested %d log entries, want 3 and at least 3", outer, nested)
	}
}
