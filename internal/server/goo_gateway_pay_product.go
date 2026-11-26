package server

import (
	"html/template"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// "Installment options" under a product's price: a collapsed <details> that shows what the store's bank POS
// gateways would charge for the product in installments. It is informative only: the real amount is
// calculated at checkout on the order total (see gooPayInstBase).

// gooPayProductOpt is the store option (Settings › Products, stored as "goocommerce_" + this) that
// turns the block off; it is on by default.
const gooPayProductOpt = "installments_on_product"

func init() {
	gooOnHTML("single_before_add_to_cart", gooPayProductInstallments)
}

// gooPayProductGateways are the enabled bridge gateways whose store installments are on with a plan.
// It reads options only, so a store without installments pays nothing more on a product page.
func (s *Server) gooPayProductGateways() []*gooGatewayHandle {
	var out []*gooGatewayHandle
	for _, g := range s.gooGatewaysOrdered() {
		pg, ok := g.gooGatewayDef.(*gooPayGateway)
		if !ok || !g.Enabled() || !pg.storeInstallments() || s.gooGatewaySetting(g.ID(), "installments") != "yes" {
			continue
		}
		if len(gooPayParsePlan(s.gooGatewaySetting(g.ID(), "installment_plan"))) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// gooPayProductPrice is the price the customer sees for the product, and whether it is a "from" price
// (a variable product's lowest variation). ok is false for types and states without one price to show.
func (s *Server) gooPayProductPrice(c *gin.Context, p *gooProduct) (price float64, from, ok bool) {
	switch p.Type {
	case gooTypeSimple:
		if pr := s.gooActive(c, p); pr != nil {
			return s.gooToDisplay(c, p, *pr), false, true
		}
	case gooTypeVariable:
		for _, v := range s.gooVariationsCached(c, p) {
			pr := s.gooActive(c, v)
			if pr == nil {
				continue
			}
			if d := s.gooToDisplay(c, v, *pr); !ok || d < price {
				price, ok = d, true
			}
		}
		return price, true, ok
	}
	return 0, false, false
}

func gooPayProductInstallments(s *Server, c *gin.Context, p *gooProduct) template.HTML {
	if p == nil || !strings.EqualFold(s.gooCurrency(), gooPayInstCurrency) {
		return ""
	}
	if v := s.gooOpt(gooPayProductOpt); v != nil && v != "" && !s.gooOptBool(gooPayProductOpt) {
		return ""
	}
	gws := s.gooPayProductGateways()
	if len(gws) == 0 {
		return ""
	}
	price, from, ok := s.gooPayProductPrice(c, p)
	if !ok || price <= 0 {
		return ""
	}
	ctx := c.Request.Context()
	var tables strings.Builder
	for _, g := range gws {
		plan := g.gooGatewayDef.(*gooPayGateway).installmentPlan(s, gooPayInstCurrency, price)
		if len(plan) == 0 {
			continue
		}
		title := g.Title()
		tables.WriteString(`<table class="goo-installments__table"><caption>` + gooEsc(title) + s.gooGatewayIconHTML(ctx, g, title) + `</caption><tbody>`)
		for _, e := range plan {
			total := price + s.gooPayInstSurcharge(price, e)
			monthly := gooRound(total/float64(e.Count), s.gooDecimals())
			tables.WriteString(`<tr><th scope="row">` + strconv.Itoa(e.Count) + ` ` + gooEsc(s.gooPayT(ctx, "installments")) + `</th><td>`)
			if from {
				tables.WriteString(gooEsc(s.gooPayT(ctx, "from")) + ` `)
			}
			tables.WriteString(string(s.gooPrice(monthly)) + ` ` + gooEsc(s.gooPayT(ctx, "per month")) + `</td><td>` +
				gooEsc(s.gooPayT(ctx, "Total")) + ` ` + string(s.gooPrice(total)))
			if e.Rate <= 0 {
				tables.WriteString(` <span class="goo-installments__free">` + gooEsc(s.gooPayT(ctx, "no surcharge")) + `</span>`)
			}
			tables.WriteString(`</td></tr>`)
		}
		tables.WriteString(`</tbody></table>`)
	}
	if tables.Len() == 0 {
		return ""
	}
	return template.HTML(`<details class="goo-installments"><summary>` + gooEsc(s.gooPayT(ctx, "Installment options")) + `</summary>` + tables.String() +
		`<p class="goo-installments__note">` + gooEsc(s.gooPayT(ctx, "The final amount is calculated at checkout on the order total.")) + `</p></details>`)
}
