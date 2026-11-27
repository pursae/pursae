//go:build sandbox

package est_test

import (
	"testing"

	"wpgo/internal/pay"
	"wpgo/internal/pay/est"
	"wpgo/internal/pay/paytest"
)

// Sandbox tests against the provider's real test endpoint; see docs/developer/payments-sandbox.md.

func specs() []pay.Spec { return est.Specs() }

func TestSandboxStart(t *testing.T)        { paytest.Start(t, specs()...) }
func TestSandboxQueryUnknown(t *testing.T) { paytest.QueryUnknown(t, specs()...) }
func TestSandboxComplete(t *testing.T)     { paytest.Complete(t, specs()...) }
func TestSandboxRefund(t *testing.T)       { paytest.Refund(t, specs()...) }
