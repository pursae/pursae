// Command hostbench is a test plugin that makes N host calls of one kind in a single hook call, so
// BenchmarkHostCall can measure the cost of a guest → host call. Built by the internal/plugins tests.
package main

import (
	"unsafe"

	"wpgo/sdk/go/wpgo"
)

func init() {
	// args: kind ("option", "translate", "raw", anything else = no call), n.
	wpgo.AddFilter("hostbench", 10, func(_ wpgo.Context, v string, args []any) string {
		kind, _ := args[0].(string)
		n, _ := args[1].(float64)
		for i := 0; i < int(n); i++ {
			switch kind {
			case "option":
				_, _, _ = wpgo.GetOption("blogname")
			case "translate":
				_ = wpgo.T("Hello")
			case "raw":
				rawOption(rawReq)
			}
		}
		return v
	})
}

var rawReq = []byte(`{"name":"blogname"}`)

//go:wasmimport wpgo get_option
func hostGetOptionRaw(ptr, size uint32) uint64

// rawOption calls get_option with a ready-made request and ignores the response (it stays pinned in
// the SDK's buffer table), measuring the call without any JSON work in the guest.
func rawOption(b []byte) {
	hostGetOptionRaw(uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b)))
}

func main() {}
