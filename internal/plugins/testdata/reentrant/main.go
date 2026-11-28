// Command reentrant is a test plugin written without the SDK whose wpgo_alloc calls a host import,
// so the host's response to that call needs wpgo_alloc again while it is still running. The
// runtime must not re-enter the guest function it is running (TestNestedAllocGetsFreshFunction).
package main

import (
	"sync"
	"unsafe"
)

var (
	mu     sync.Mutex
	pinned = map[uint32][]byte{}
	depth  int
)

//go:wasmimport wpgo log
func hostLog(ptr, size uint32) uint64

func logMsg(msg string) {
	b := []byte(`{"level":"info","message":"` + msg + `"}`)
	hostLog(uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b)))
}

//go:wasmexport wpgo_alloc
func wpgoAlloc(size uint32) uint32 {
	if depth == 0 {
		depth++
		logMsg("nested")
		depth--
	}
	b := make([]byte, size+1)
	ptr := uint32(uintptr(unsafe.Pointer(&b[0])))
	mu.Lock()
	pinned[ptr] = b
	mu.Unlock()
	return ptr
}

//go:wasmexport wpgo_free
func wpgoFree(ptr, size uint32) {
	mu.Lock()
	delete(pinned, ptr)
	mu.Unlock()
}

//go:wasmexport wpgo_init
func wpgoInit() {
	for i := 0; i < 3; i++ {
		logMsg("outer")
	}
}

//go:wasmexport wpgo_handle
func wpgoHandle(ptr, size uint32) uint64 {
	out := []byte(`{"error":"no callbacks"}`)
	p := wpgoAlloc(uint32(len(out)))
	copy(pinned[p], out)
	return uint64(p)<<32 | uint64(len(out))
}

func main() {}
