package server

import (
	"context"
	"errors"
	"html/template"
	"log"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/pay"
)

// The hosted-payment bridge on the admin order screen: the payment attempts of the order (so the
// merchant can find the payment in the provider's panel) and the "Check payment status with …"
// order action, which asks the provider now through the same locked apply path as the pending
// resolver (gooPayResolve).

// gooPayCheckAction is the order action's id (wc_order_action).
const gooPayCheckAction = "goo_pay_check_status"

// gooOrderActionNoticeKey carries an order action's outcome to the order screen's notice.
const gooOrderActionNoticeKey = "goo.order_action_notice"

// gooPayCheckAttempt is the attempt the status check asks about: the order's newest started attempt,
// else its newest paid one.
func (s *Server) gooPayCheckAttempt(ctx context.Context, orderID int64) (*gooPayAttempt, error) {
	return gooScanPayAttempt(s.store.Pool.QueryRow(ctx, `SELECT `+gooPayAttemptCols+` FROM goo_payment_attempts
		WHERE order_id = $1 AND status IN ('started', 'paid') ORDER BY (status = 'started') DESC, id DESC LIMIT 1`, orderID))
}

// gooPayOrderActions is the status check entry of the order's Order actions box, when the order has an
// attempt with a bridge gateway whose provider can be queried.
func (s *Server) gooPayOrderActions(ctx context.Context, o *gooOrder) []gooOpt {
	a, err := s.gooPayCheckAttempt(ctx, o.ID)
	if err != nil {
		return nil
	}
	g, _ := gooGatewayRegistry[a.Gateway].(*gooPayGateway)
	if g == nil || !g.has(pay.FeatureQuery) {
		return nil
	}
	return []gooOpt{{gooPayCheckAction, s.gooL10nf(ctx, "Check payment status with %s", g.spec.Title)}}
}

// gooPayRunOrderAction runs the status check; handled is false for other actions. A returned error
// is shown as an error notice; other outcomes as the action's notice (gooOrderActionNoticeKey).
func (s *Server) gooPayRunOrderAction(c *gin.Context, o *gooOrder, act string) (handled bool, err error) {
	if act != gooPayCheckAction {
		return false, nil
	}
	msg, err := s.gooPayCheckStatus(c.Request.Context(), o, s.gooAdminUserName(c))
	if err != nil {
		return true, err
	}
	c.Set(gooOrderActionNoticeKey, msg)
	return true, nil
}

// gooPayCheckStatus asks the provider about the order's open (or paid) attempt and applies the answer.
func (s *Server) gooPayCheckStatus(ctx context.Context, o *gooOrder, by string) (string, error) {
	a, err := s.gooPayCheckAttempt(ctx, o.ID)
	if err != nil {
		if gooNotFound(err) {
			return "", errors.New(s.gooL10nText(ctx, "This order has no payment to check."))
		}
		return "", err
	}
	g, _ := gooGatewayRegistry[a.Gateway].(*gooPayGateway)
	if g == nil {
		return "", errors.New(s.gooL10nf(ctx, "The payment method %s is not available.", a.Gateway))
	}
	title := g.spec.Title
	if !g.has(pay.FeatureQuery) {
		return "", errors.New(s.gooL10nf(ctx, "%s does not support payment status checks. Check the payment in the %s panel.", title, title))
	}
	p, err := g.provider(s, a.Test)
	if err != nil {
		return "", errors.New(s.gooL10nf(ctx, "%s is not configured: %s", title, err.Error()))
	}
	wasPaid := a.Status == "paid"
	var res *pay.Result
	var qerr error
	err = s.gooPayLocked(ctx, a, func(ctx context.Context, db gooPayDB, a *gooPayAttempt) error {
		r, err := p.Query(ctx, a.toPay())
		if err != nil || r == nil {
			if err == nil {
				err = errors.New("no result")
			}
			qerr = err
			return nil
		}
		res = r
		return s.gooPayApply(ctx, db, g, a, r)
	})
	if err != nil {
		log.Printf("goocommerce: %s: status check of attempt %s: %v", g.ID(), a.Ref, err)
		return "", errors.New(s.gooL10nf(ctx, "The answer of %s could not be stored. Try again.", title))
	}
	var msg string
	switch {
	case errors.Is(qerr, pay.ErrNotSupported):
		return "", errors.New(s.gooL10nf(ctx, "%s does not support status checks for this payment. Check it in the %s panel (attempt %s).", title, title, a.Ref))
	case qerr != nil:
		log.Printf("goocommerce: %s: status check of attempt %s: %q", g.ID(), a.Ref, qerr.Error())
		return "", errors.New(s.gooL10nf(ctx, "%s could not be reached. Try again in a few minutes (attempt %s).", title, a.Ref))
	case res.Status == pay.StatusSuccess && a.Code == "amount_mismatch":
		msg = s.gooL10nf(ctx, "%s reports a payment that does not match the order; the order is on hold for review (attempt %s).", title, a.Ref)
	case res.Status == pay.StatusSuccess && wasPaid:
		msg = s.gooL10nf(ctx, "%s confirms the payment (attempt %s).", title, a.Ref)
	case res.Status == pay.StatusSuccess:
		msg = s.gooL10nf(ctx, "Paid: %s confirmed the payment and the order was marked paid (attempt %s).", title, a.Ref)
	case res.Status == pay.StatusFailure || res.Status == pay.StatusCancelled:
		reason := s.gooPayT(ctx, pay.Message(res.Code))
		if d := strings.TrimSpace(res.ProviderCode + " " + pay.Trunc(res.ProviderMessage, 200)); d != "" {
			reason += " (" + d + ")"
		}
		_ = s.gooAddOrderNote(ctx, o, gooEsc(s.gooL10nf(ctx, "Payment status checked with %s: failed. %s", title, reason)), false, by)
		return "", errors.New(s.gooL10nf(ctx, "The payment failed: %s (attempt %s).", reason, a.Ref))
	default:
		msg = s.gooL10nf(ctx, "Still waiting: %s has not confirmed the payment yet (attempt %s).", title, a.Ref)
	}
	_ = s.gooAddOrderNote(ctx, o, gooEsc(s.gooL10nf(ctx, "Payment status checked with %s: %s", title, msg)), false, by)
	return msg, nil
}

// gooPayOrderInfo is the order screen's box of the order's payment attempts (newest first, at most
// five): reference, mode, status, amount and installments.
func (s *Server) gooPayOrderInfo(ctx context.Context, o *gooOrder) template.HTML {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+gooPayAttemptCols+` FROM goo_payment_attempts WHERE order_id = $1 ORDER BY id DESC LIMIT 5`, o.ID)
	if err != nil {
		return ""
	}
	var list []*gooPayAttempt
	for rows.Next() {
		if a, err := gooScanPayAttempt(rows); err == nil {
			list = append(list, a)
		}
	}
	rows.Close()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="goo-pay-attempts"><p><strong>` + gooEsc(s.gooL10nText(ctx, "Payment attempts")) + `</strong></p><ul>`)
	for _, a := range list {
		title := a.Gateway
		if g, _ := gooGatewayRegistry[a.Gateway].(*gooPayGateway); g != nil {
			title = g.spec.Title
		}
		mode := "live mode"
		if a.Test {
			mode = "test mode"
		}
		parts := []string{title, s.gooL10nf(ctx, "reference %s", a.Ref)}
		if a.ProviderRef != "" {
			parts = append(parts, s.gooL10nf(ctx, "provider reference %s", a.ProviderRef))
		}
		if a.TxnID != "" {
			parts = append(parts, s.gooL10nf(ctx, "transaction %s", a.TxnID))
		}
		parts = append(parts, s.gooL10nText(ctx, mode), s.gooL10nText(ctx, a.Status), pay.FormatMinor(a.Amount, a.Currency)+" "+a.Currency)
		if a.Installment > 1 {
			parts = append(parts, s.gooL10nf(ctx, "%s installments", strconv.Itoa(a.Installment)))
		}
		if a.Code != "" && a.Status != "paid" {
			parts = append(parts, a.Code)
		}
		b.WriteString(`<li>` + gooEsc(strings.Join(parts, " · ")) + ` <small>` + gooEsc(a.Created.In(s.loc()).Format("2006-01-02 15:04")) + `</small></li>`)
	}
	b.WriteString(`</ul></div>`)
	return template.HTML(b.String())
}
