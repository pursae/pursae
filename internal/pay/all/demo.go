//go:build paydemo

package all

import "wpgo/internal/pay/demo"

// The demo bank exists only in development builds (go build -tags paydemo).
func init() { extra = append(extra, demo.Spec) }
