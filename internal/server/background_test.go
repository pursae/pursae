package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAsyncWriterBounded: the queue never grows past its size (overflow is dropped and counted),
// and close runs what was queued before returning.
func TestAsyncWriterBounded(t *testing.T) {
	var a asyncWriter
	block := make(chan struct{})
	var ran atomic.Int64
	var started sync.WaitGroup
	started.Add(asyncWorkers)
	for range asyncWorkers { // occupy the workers
		a.submit(func(context.Context) { started.Done(); <-block; ran.Add(1) })
	}
	started.Wait() // every worker holds one write, so the queue is empty
	accepted := 0
	for i := 0; i < asyncQueueSize+100; i++ {
		if a.submit(func(context.Context) { ran.Add(1) }) {
			accepted++
		}
	}
	if accepted != asyncQueueSize || a.Dropped() != 100 {
		t.Fatalf("accepted %d dropped %d, want %d and 100", accepted, a.Dropped(), asyncQueueSize)
	}
	close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.close(ctx)
	if got := ran.Load(); got != int64(asyncQueueSize+asyncWorkers) {
		t.Fatalf("ran %d writes, want all %d queued", got, asyncQueueSize+asyncWorkers)
	}
	if a.submit(func(context.Context) {}) {
		t.Fatal("submit after close accepted")
	}
	a.close(ctx) // idempotent
}

// TestAsyncWriterCloseUnused: closing a writer that never started does not block.
func TestAsyncWriterCloseUnused(t *testing.T) {
	var a asyncWriter
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a.close(ctx)
	if ctx.Err() != nil {
		t.Fatal("close of an unused writer waited for its context")
	}
}

// TestCDNPurgeCoalesced: purges requested close together become one call; a request for
// everything wins over URL lists; flush runs a pending purge at shutdown.
func TestCDNPurgeCoalesced(t *testing.T) {
	s := &Server{}
	var mu sync.Mutex
	var calls [][]string
	s.rt.cdn.delay = 50 * time.Millisecond
	s.rt.cdn.run = func(_ context.Context, _ gocdnSettings, urls []string) {
		mu.Lock()
		calls = append(calls, urls)
		mu.Unlock()
	}
	// waitCall waits for the timer's purge instead of sleeping a fixed time (slow or -race runs).
	waitCall := func() {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			mu.Lock()
			n := len(calls)
			mu.Unlock()
			if n > 0 {
				s.rt.cdn.wg.Wait() // let the drain loop finish, or the next batch joins this run
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("no purge ran")
			}
		}
	}
	s.gocdnQueuePurge(gocdnSettings{}, []string{"/a", "/b"})
	s.gocdnQueuePurge(gocdnSettings{}, []string{"/b", "/c"})
	waitCall()
	mu.Lock()
	if len(calls) != 1 || len(calls[0]) != 3 {
		t.Fatalf("calls %v, want one purge of /a /b /c", calls)
	}
	calls = nil
	mu.Unlock()

	s.gocdnQueuePurge(gocdnSettings{}, []string{"/x"})
	s.gocdnQueuePurge(gocdnSettings{}, nil)
	s.gocdnQueuePurge(gocdnSettings{}, []string{"/y"})
	waitCall()
	mu.Lock()
	if len(calls) != 1 || calls[0] != nil {
		t.Fatalf("calls %v, want one purge of everything", calls)
	}
	calls = nil
	mu.Unlock()

	s.rt.cdn.delay = time.Hour
	s.gocdnQueuePurge(gocdnSettings{}, []string{"/z"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.Close(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || len(calls[0]) != 1 {
		t.Fatalf("flush: calls %v, want the pending purge of /z", calls)
	}
}
