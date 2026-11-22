package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestSlowQueryTracer(t *testing.T) {
	var n int
	tr := &SlowQueryTracer{Threshold: 5 * time.Millisecond, OnSlow: func() { n++ }}
	run := func(d time.Duration) {
		ctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT   1\n FROM t", Args: []any{"secret"}})
		time.Sleep(d)
		tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	}
	run(0)
	if n != 0 {
		t.Fatal("fast query counted")
	}
	run(15 * time.Millisecond)
	if n != 1 {
		t.Fatal("slow query not counted")
	}
	tr.TraceQueryEnd(context.Background(), nil, pgx.TraceQueryEndData{}) // no start in ctx: ignored
}

func TestShortSQL(t *testing.T) {
	if got := shortSQL("SELECT  1\n\tFROM t"); got != "SELECT 1 FROM t" {
		t.Errorf("got %q", got)
	}
	if got := shortSQL(strings.Repeat("é", 400)); len(got) > 310 || !strings.HasSuffix(got, "…") {
		t.Errorf("not cut: %d", len(got))
	}
}
