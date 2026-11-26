// Package all lists the hosted-payment providers Pursae Commerce offers. The Commerce bridge
// (internal/server, goo_gateway_pay.go) registers one gateway per Spec.
package all

import (
	"wpgo/internal/pay"
	"wpgo/internal/pay/akbank"
	"wpgo/internal/pay/est"
	"wpgo/internal/pay/garanti"
	"wpgo/internal/pay/iyzico"
	"wpgo/internal/pay/paypal"
	"wpgo/internal/pay/paytr"
	"wpgo/internal/pay/qnb"
	"wpgo/internal/pay/sipay"
	"wpgo/internal/pay/vakifbank"
)

// extra are providers added by build-tagged files: the development-only demo bank (demo.go, tag paydemo).
var extra []func() pay.Spec

// Specs are the providers, in the order Settings › Payments shows them inside each group: payment
// service providers first, then the banks.
//
// Not here because no hosted mode (card typed on the provider's page) could be confirmed from a source:
// Yapı Kredi, DenizBank, Param, QNBpay, Kuveyt Türk, Albaraka Türk, and Garanti BBVA's common payment
// page (only GarantiPay is offered for Garanti).
func Specs() []pay.Spec {
	out := []pay.Spec{paytr.Spec(), iyzico.Spec()}
	out = append(out, sipay.Specs()...)
	out = append(out, est.Specs()...)
	out = append(out, akbank.Spec(), garanti.Spec(), qnb.Spec(), vakifbank.Spec(), paypal.Spec())
	for _, f := range extra {
		out = append(out, f())
	}
	return out
}
