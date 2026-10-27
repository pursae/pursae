//go:build wasip1 && wasm

package wpgo

import (
	"runtime"
	"sync"
	"unsafe"
)

// Guest memory management.
//
// Buffers handed across the boundary are ordinary Go byte slices. Go's
// garbage collector does not move objects, so a slice's address is stable as
// long as the slice is reachable; pinned keeps every outstanding buffer
// reachable until it is released with wpgo_free (host side) or takeBuffer
// (guest side).
var (
	pinMu  sync.Mutex
	pinned = map[uint32][]byte{}
)

func pin(b []byte) uint32 {
	if len(b) == 0 {
		b = make([]byte, 1)[:0:1]
	}
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b))))
	pinMu.Lock()
	pinned[ptr] = b
	pinMu.Unlock()
	return ptr
}

// takeBuffer unpins and returns the buffer at ptr, trimmed to size. The host
// always writes into buffers it obtained from wpgo_alloc, so they are returned
// without copying; an unknown pointer yields nil.
func takeBuffer(ptr, size uint32) []byte {
	pinMu.Lock()
	b, ok := pinned[ptr]
	delete(pinned, ptr)
	pinMu.Unlock()
	if !ok || uint32(cap(b)) < size {
		return nil
	}
	return b[:size]
}

func pack(ptr, size uint32) uint64 { return uint64(ptr)<<32 | uint64(size) }

func unpack(v uint64) (ptr, size uint32) { return uint32(v >> 32), uint32(v) }

//go:wasmexport wpgo_alloc
func wpgoAlloc(size uint32) uint32 {
	return pin(make([]byte, size))
}

//go:wasmexport wpgo_free
func wpgoFree(ptr, size uint32) {
	pinMu.Lock()
	delete(pinned, ptr)
	pinMu.Unlock()
}

//go:wasmexport wpgo_init
func wpgoInit() {
	runInit()
}

//go:wasmexport wpgo_handle
func wpgoHandle(ptr, size uint32) uint64 {
	req := takeBuffer(ptr, size)
	out := Dispatch(req)
	return pack(pin(out), uint32(len(out)))
}

//go:wasmimport wpgo add_hook
func hostAddHook(ptr, size uint32) uint64

//go:wasmimport wpgo get_option
func hostGetOption(ptr, size uint32) uint64

//go:wasmimport wpgo update_option
func hostUpdateOption(ptr, size uint32) uint64

//go:wasmimport wpgo log
func hostLog(ptr, size uint32) uint64

//go:wasmimport wpgo http_get
func hostHTTPGet(ptr, size uint32) uint64

//go:wasmimport wpgo storage_get
func hostStorageGet(ptr, size uint32) uint64

//go:wasmimport wpgo storage_set
func hostStorageSet(ptr, size uint32) uint64

//go:wasmimport wpgo add_admin_notice
func hostAddAdminNotice(ptr, size uint32) uint64

//go:wasmimport wpgo register_shortcode
func hostRegisterShortcode(ptr, size uint32) uint64

//go:wasmimport wpgo register_rest_route
func hostRegisterRestRoute(ptr, size uint32) uint64

//go:wasmimport wpgo get_post
func hostGetPost(ptr, size uint32) uint64

//go:wasmimport wpgo query_posts
func hostQueryPosts(ptr, size uint32) uint64

//go:wasmimport wpgo insert_post
func hostInsertPost(ptr, size uint32) uint64

//go:wasmimport wpgo update_post
func hostUpdatePost(ptr, size uint32) uint64

//go:wasmimport wpgo get_current_user
func hostGetCurrentUser(ptr, size uint32) uint64

//go:wasmimport wpgo schedule_event
func hostScheduleEvent(ptr, size uint32) uint64

//go:wasmimport wpgo unschedule_event
func hostUnscheduleEvent(ptr, size uint32) uint64

//go:wasmimport wpgo register_settings_page
func hostRegisterSettingsPage(ptr, size uint32) uint64

//go:wasmimport wpgo transient_get
func hostTransientGet(ptr, size uint32) uint64

//go:wasmimport wpgo transient_set
func hostTransientSet(ptr, size uint32) uint64

//go:wasmimport wpgo transient_delete
func hostTransientDelete(ptr, size uint32) uint64

//go:wasmimport wpgo enqueue_style
func hostEnqueueStyle(ptr, size uint32) uint64

//go:wasmimport wpgo enqueue_script
func hostEnqueueScript(ptr, size uint32) uint64

var imports = map[string]func(ptr, size uint32) uint64{
	"add_hook":               hostAddHook,
	"get_option":             hostGetOption,
	"update_option":          hostUpdateOption,
	"log":                    hostLog,
	"http_get":               hostHTTPGet,
	"storage_get":            hostStorageGet,
	"storage_set":            hostStorageSet,
	"add_admin_notice":       hostAddAdminNotice,
	"register_shortcode":     hostRegisterShortcode,
	"register_rest_route":    hostRegisterRestRoute,
	"get_post":               hostGetPost,
	"query_posts":            hostQueryPosts,
	"insert_post":            hostInsertPost,
	"update_post":            hostUpdatePost,
	"get_current_user":       hostGetCurrentUser,
	"schedule_event":         hostScheduleEvent,
	"unschedule_event":       hostUnscheduleEvent,
	"register_settings_page": hostRegisterSettingsPage,
	"transient_get":          hostTransientGet,
	"transient_set":          hostTransientSet,
	"transient_delete":       hostTransientDelete,
	"enqueue_style":          hostEnqueueStyle,
	"enqueue_script":         hostEnqueueScript,
}

func invokeHost(name string, body []byte) ([]byte, error) {
	fn, ok := imports[name]
	if !ok {
		return nil, &Error{Code: CodeInvalidRequest, Message: "unknown host function " + name}
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	packed := fn(uint32(uintptr(unsafe.Pointer(unsafe.SliceData(body)))), uint32(len(body)))
	runtime.KeepAlive(body)
	if packed == 0 {
		return nil, nil
	}
	return takeBuffer(unpack(packed)), nil
}
