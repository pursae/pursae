package server

import (
	"context"
	"io/fs"
	"regexp"
)

// Payment method logos: one SVG (or, where the company publishes no SVG, a PNG) per gateway id in the
// public static directory of Pursae Commerce (the checkout shows them to shoppers who are not logged in).
// An id without a file gets generic.svg.

const gooGatewayLogoDir = "images/payment-gateways"

var gooGatewayLogoID = regexp.MustCompile(`^[a-z0-9_-]+$`)

// gooGatewayLogoURL is the absolute URL of a gateway's logo, or of the generic card tile.
func (s *Server) gooGatewayLogoURL(ctx context.Context, id string) string {
	file := "generic.svg"
	if gooGatewayLogoID.MatchString(id) {
		for _, ext := range []string{".svg", ".png"} {
			if _, err := fs.Stat(s.web, "goocommerce/assets/"+gooGatewayLogoDir+"/"+id+ext); err == nil {
				file = id + ext
				break
			}
		}
	}
	return s.links(ctx).HomeURL(gooAssets + "/" + gooGatewayLogoDir + "/" + file)
}

// gooGatewayShowsLogo reports whether the checkout shows a logo for the gateway: the offline methods
// (bank transfer, check, cash on delivery) have none.
func gooGatewayShowsLogo(g gooGatewayDef) bool {
	return gooGatewayInfoOf(g).Region != gooRegionOffline
}

// gooGatewayIconHTML is the <img> after a method title on the checkout (WC_Payment_Gateway::get_icon()).
func (s *Server) gooGatewayIconHTML(ctx context.Context, g *gooGatewayHandle, title string) string {
	if !gooGatewayShowsLogo(g) {
		return ""
	}
	return ` <img class="goo-gateway-icon" src="` + gooEsc(s.gooGatewayLogoURL(ctx, g.ID())) + `" alt="` + gooEsc(title) + `" height="28" loading="lazy" />`
}
