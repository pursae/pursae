//go:build !paydemo

package server

// gooPayAllowLoopbackHTTP lets a provider's form, iframe or redirect use http://127.0.0.1. Only the
// development build with the demo provider (build tag paydemo) sets it; release builds refuse every
// URL that is not https on a listed host.
const gooPayAllowLoopbackHTTP = false
