//go:build paydemo

package server

// gooPayAllowLoopbackHTTP: the demo provider (internal/pay/demo, build tag paydemo) serves its
// hosted page on http://127.0.0.1, so this development build accepts that one origin.
const gooPayAllowLoopbackHTTP = true
