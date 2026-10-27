package wpgo

import (
	"encoding/json"
	"fmt"
	"sync"
)

// DefaultPriority is the WordPress default hook priority.
const DefaultPriority = 10

// Context describes the hook invocation a callback is serving.
type Context struct {
	// Plugin is the slug of the running plugin.
	Plugin string
	// Hook is the hook name, e.g. "the_content".
	Hook string
	// Callback is the SDK-generated callback identifier.
	Callback string
}

// FilterFunc is a typed filter callback. It receives the current value and any
// extra arguments the core passed to ApplyFilters and returns the new value.
type FilterFunc[T any] func(ctx Context, value T, args []any) T

// ActionFunc is an action callback. It receives the arguments the core passed
// to DoAction.
type ActionFunc func(ctx Context, args []any)

// rawCallback is the type-erased form stored in the callback table. args are
// the decoded extra arguments, raw the same arguments as JSON.
type rawCallback func(ctx Context, value json.RawMessage, args []any, raw []json.RawMessage) (json.RawMessage, error)

// hookRegistration mirrors the JSON accepted by the host's add_hook import.
type hookRegistration struct {
	Type     string `json:"type"`
	Hook     string `json:"hook"`
	Priority int    `json:"priority"`
	Callback string `json:"callback"`
}

var (
	mu          sync.Mutex
	callbacks   = map[string]rawCallback{}
	pending     []hookRegistration
	pendingRegs []func() // other registrations (shortcodes, routes, …) queued until init
	initFuncs   []func()
	initialized bool
	nextID      int
)

// AddFilter registers fn as a filter for hook at the given priority (lower
// runs earlier; use DefaultPriority when in doubt). The value is decoded from
// JSON into T, so T should match what the core passes (string for
// "the_content", map[string]any or a struct for structured data).
//
// If fn's value cannot be decoded into T, the host receives an error and keeps
// the original value.
func AddFilter[T any](hook string, priority int, fn FilterFunc[T]) {
	register("filter", hook, priority, func(ctx Context, raw json.RawMessage, args []any, _ []json.RawMessage) (json.RawMessage, error) {
		var v T
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("decode value for %s: %w", ctx.Hook, err)
			}
		}
		return json.Marshal(fn(ctx, v, args))
	})
}

// AddAction registers fn as an action for hook at the given priority.
func AddAction(hook string, priority int, fn ActionFunc) {
	register("action", hook, priority, func(ctx Context, _ json.RawMessage, args []any, _ []json.RawMessage) (json.RawMessage, error) {
		fn(ctx, args)
		return nil, nil
	})
}

// OnInit registers fn to run once when the host initialises the plugin (after
// hooks registered in package init functions have been sent to the host).
// Host functions such as GetOption may be used inside fn.
func OnInit(fn func()) {
	mu.Lock()
	initFuncs = append(initFuncs, fn)
	mu.Unlock()
}

func register(kind, hook string, priority int, cb rawCallback) {
	mu.Lock()
	nextID++
	reg := hookRegistration{Type: kind, Hook: hook, Priority: priority, Callback: fmt.Sprintf("%s#%d", hook, nextID)}
	callbacks[reg.Callback] = cb
	live := initialized
	if !live {
		pending = append(pending, reg)
	}
	mu.Unlock()
	if live {
		if err := sendRegistration(reg); err != nil {
			Log("error", "add_hook "+hook+": "+err.Error())
		}
	}
}

// addCallback stores cb under a fresh name "<kind>:<name>#<n>" and returns it.
func addCallback(kind, name string, cb rawCallback) string {
	mu.Lock()
	defer mu.Unlock()
	nextID++
	id := fmt.Sprintf("%s:%s#%d", kind, name, nextID)
	callbacks[id] = cb
	return id
}

// queueRegistration runs fn now when the host has initialised the plugin, or
// queues it until wpgo_init otherwise.
func queueRegistration(fn func()) {
	mu.Lock()
	live := initialized
	if !live {
		pendingRegs = append(pendingRegs, fn)
	}
	mu.Unlock()
	if live {
		fn()
	}
}

func lookupCallback(name string) (rawCallback, bool) {
	mu.Lock()
	defer mu.Unlock()
	cb, ok := callbacks[name]
	return cb, ok
}

// Init flushes queued hook registrations and runs OnInit callbacks. Inside
// the host this happens automatically through the wpgo_init export, so plugins
// never need to call it; it exists for unit tests run outside the host.
// Calling it more than once has no effect.
func Init() { runInit() }

// runInit flushes queued hook registrations to the host and runs OnInit
// callbacks. It is what the wpgo_init export does.
func runInit() {
	mu.Lock()
	if initialized {
		mu.Unlock()
		return
	}
	initialized = true
	regs := pending
	pending = nil
	others := pendingRegs
	pendingRegs = nil
	fns := initFuncs
	mu.Unlock()

	for _, reg := range regs {
		if err := sendRegistration(reg); err != nil {
			Log("error", "add_hook "+reg.Hook+": "+err.Error())
		}
	}
	for _, fn := range others {
		fn()
	}
	for _, fn := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					Log("error", fmt.Sprintf("panic in OnInit callback: %v", r))
				}
			}()
			fn()
		}()
	}
}

func sendRegistration(reg hookRegistration) error {
	_, err := hostCall("add_hook", reg)
	return err
}
