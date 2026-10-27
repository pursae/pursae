// Package wpgo is the plugin-author SDK for WordPress Go (wpgo).
//
// A wpgo plugin is an ordinary Go program compiled to WebAssembly:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
//
// The SDK hides the host ABI (JSON messages exchanged through guest memory,
// the wpgo_* exports and the "wpgo" host imports) behind a small, WordPress
// flavoured API. Hooks are usually registered from a package init function:
//
//	package main
//
//	import (
//		"strings"
//
//		"wpgo/sdk/go/wpgo"
//	)
//
//	func init() {
//		wpgo.AddFilter("the_content", 10, func(ctx wpgo.Context, value string, args []any) string {
//			return strings.ToUpper(value)
//		})
//	}
//
//	func main() {}
//
// Registrations made during init are queued and sent to the host when it calls
// the wpgo_init export, right after the module has been instantiated. Callbacks
// registered with OnInit run at that point too, after the queued hooks, and may
// already use host functions such as GetOption.
//
// Beyond hooks, plugins register shortcodes (AddShortcode), REST routes under
// their own namespace (RegisterRestRoute), a settings page the admin renders
// (RegisterSettingsPage), scheduled events (ScheduleEvent) and package assets
// (EnqueueStyle, EnqueueScript), and read or write posts (GetPost, QueryPosts,
// InsertPost, UpdatePost), the current user (CurrentUser), options, private
// storage and transients.
//
// Host functions (GetOption, UpdateOption, HTTPGet, Storage, AdminNotice, ...)
// are subject to the permissions declared in plugin.json and approved by the
// site administrator. A call the plugin is not allowed to make returns an
// *Error whose Code is CodePermissionDenied; see IsPermissionDenied.
//
// Panics inside hook callbacks are recovered by the SDK and reported to the
// host as callback failures, so a buggy callback does not take the whole
// module down. The host keeps the original filter value in that case.
//
// When the package is compiled for any target other than wasip1/wasm (for
// example while running `go vet` or unit tests on a developer machine) the hook
// registration functions still work, and every host function returns
// ErrNotInHost. Use Dispatch to exercise callbacks from ordinary Go tests.
package wpgo
