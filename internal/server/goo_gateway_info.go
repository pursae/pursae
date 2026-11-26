package server

// Where a payment method belongs in Settings › Payments and what it can do. Settings › Payments groups
// the methods by region (Türkiye, Worldwide, Offline) and shows each one with its logo and features;
// the checkout and the REST API show the logo.

// Gateway regions.
const (
	gooRegionTR      = "tr"      // Türkiye: payment service providers and bank virtual POS
	gooRegionGlobal  = "global"  // Worldwide: Stripe, PayPal
	gooRegionOffline = "offline" // bank transfer, check, cash on delivery
	gooRegionTest    = "test"    // the test gateway
)

// gooGatewayInfo describes a gateway for the settings screen.
type gooGatewayInfo struct {
	Region   string   // gooRegion*
	Kind     string   // "psp", "bank", "wallet", "offline", "test"
	Features []string // "3ds", "installments", "refunds", "subscriptions", "saved_cards", "query"
	// Verified is true once the gateway was tested against the provider with a real test merchant.
	// The settings screen marks the others "Beta".
	Verified bool
	DocsURL  string
}

// gooGatewayInfoer is implemented by gateways that describe themselves. The others get
// gooGatewayInfoOf's defaults.
type gooGatewayInfoer interface {
	Info() gooGatewayInfo
}

// gooGatewayInfoOf is a gateway's info: its own, or the default of the built-in ones.
func gooGatewayInfoOf(g gooGatewayDef) gooGatewayInfo {
	if h, ok := g.(*gooGatewayHandle); ok && h != nil {
		g = h.gooGatewayDef
	}
	if i, ok := g.(gooGatewayInfoer); ok {
		return i.Info()
	}
	switch g.ID() {
	case "bacs", "cheque", "cod":
		return gooGatewayInfo{Region: gooRegionOffline, Kind: "offline", Verified: true}
	case "goo_test":
		return gooGatewayInfo{Region: gooRegionTest, Kind: "test", Features: []string{"refunds", "subscriptions"}, Verified: true}
	case "goo_stripe":
		return gooGatewayInfo{Region: gooRegionGlobal, Kind: "psp", Verified: true, DocsURL: "https://docs.stripe.com/payments/checkout",
			Features: []string{"3ds", "refunds", "subscriptions", "saved_cards"}}
	}
	// A built-in method that is not a payment provider (the booking request of Pursae Bookings, for
	// example): it sits with the offline methods.
	return gooGatewayInfo{Region: gooRegionOffline, Kind: "offline", Verified: true}
}

// gooGatewaySetup is implemented by gateways that need credentials before they can take payments.
// SetupProblem is "" when the gateway is configured, otherwise a short English sentence for the store
// admin ("The merchant key is missing."). Settings › Payments shows "Setup incomplete" and refuses to
// enable such a gateway; the callback route (/wc-api/<id>/) answers only for gateways that are enabled
// or configured.
type gooGatewaySetup interface {
	SetupProblem(s *Server) string
}

// gooGatewaySetupProblem is a gateway's setup problem, "" for gateways that need no setup.
func (s *Server) gooGatewaySetupProblem(g gooGatewayDef) string {
	if h, ok := g.(*gooGatewayHandle); ok && h != nil {
		g = h.gooGatewayDef
	}
	if st, ok := g.(gooGatewaySetup); ok {
		return st.SetupProblem(s)
	}
	return ""
}

// gooGatewayTestMode reports whether a gateway runs in its provider's test / sandbox mode (the
// "testmode" setting, as Stripe and the hosted-payment bridge name it).
func (s *Server) gooGatewayTestMode(id string) bool {
	return s.gooGatewaySetting(id, "testmode") == "yes"
}
