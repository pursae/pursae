package plugins

import (
	"context"
	"testing"
)

// BenchmarkHostCall measures one guest → host call from an SDK plugin: a hook call makes perCall
// host calls, and ns/hostcall divides the time by them. "option" is wpgo.GetOption (also behind
// SettingString), "translate" wpgo.T, "raw" a get_option call with a ready-made request and no JSON
// work in the guest (the floor of a host call), "none" the hook call alone (host → guest).
//
//	go test ./internal/plugins -run '^$' -bench HostCall -benchtime 2s
func BenchmarkHostCall(b *testing.B) {
	rt := newTestRuntime(b, Config{})
	host := NewMemoryHost()
	_ = host.UpdateOption(context.Background(), "hostbench", "blogname", "Bench")
	loadPlugin(b, rt, "internal/plugins/testdata/hostbench", host)
	ctx := context.Background()
	const perCall = 1000
	for _, kind := range []string{"none", "raw", "option", "translate"} {
		n := perCall
		if kind == "none" {
			n = 0
		}
		b.Run(kind, func(b *testing.B) {
			rt.Hooks().ApplyFilters(ctx, "hostbench", "x", kind, n) // warm up
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := rt.Hooks().ApplyFilters(ctx, "hostbench", "x", kind, n); got != "x" {
					b.Fatalf("got %v", got)
				}
			}
			b.StopTimer()
			if n > 0 {
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/hostcall")
			}
		})
	}
}
