package main

import "testing"

func TestMetricsAddrLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:9100": true, "[::1]:9100": true, "localhost:9100": true, "127.0.0.2:9100": true,
		":9100": false, "0.0.0.0:9100": false, "[::]:9100": false, "10.0.0.1:9100": false, "metrics.example:9100": false, "9100": false,
	} {
		if got := metricsAddrLoopback(addr); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}
