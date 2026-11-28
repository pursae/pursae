package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"wpgo/internal/pay"
)

// The hosted-payment bridge's attempts, callbacks, refunds and pending resolver (goo_gateway_pay.go).

func init() {
	gooOn("hourly", 60, func(s *Server, ctx context.Context, _ any) { s.gooPayResolve(ctx) })
}

// Attempts -----------------------------------------------------------------------------------------------

// gooPayAttempt is a row of goo_payment_attempts.
type gooPayAttempt struct {
	ID          int64
	OrderID     int64
	Gateway     string
	Ref         string
	ProviderRef string
	Amount      int64
	Currency    string
	Installment int
	Test        bool
	Status      string // started | paid | failed | expired
	TxnID       string
	Paid        int64
	Refunded    int64
	Code        string
	Start       []byte
	Created     time.Time
}

const gooPayAttemptCols = `id, order_id, gateway, ref, provider_ref, amount_minor, currency, installment, test, status, txn_id,
	paid_minor, refunded_minor, code, start, created_at`

func gooScanPayAttempt(row pgx.Row) (*gooPayAttempt, error) {
	a := &gooPayAttempt{}
	err := row.Scan(&a.ID, &a.OrderID, &a.Gateway, &a.Ref, &a.ProviderRef, &a.Amount, &a.Currency, &a.Installment, &a.Test, &a.Status,
		&a.TxnID, &a.Paid, &a.Refunded, &a.Code, &a.Start, &a.Created)
	return a, err
}

// toPay is the attempt as the provider sees it.
func (a *gooPayAttempt) toPay() pay.Attempt {
	return pay.Attempt{Ref: a.Ref, ProviderRef: a.ProviderRef, Amount: a.Amount, Currency: a.Currency, Installment: a.Installment,
		Test: a.Test, CreatedAt: a.Created, PaidAmount: a.Paid, TxnID: a.TxnID}
}

func (s *Server) gooPayInsertAttempt(ctx context.Context, a *gooPayAttempt) error {
	return s.store.Pool.QueryRow(ctx, `INSERT INTO goo_payment_attempts (order_id, gateway, ref, amount_minor, currency, installment, test)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at`, a.OrderID, a.Gateway, a.Ref, a.Amount, a.Currency, a.Installment, a.Test).Scan(&a.ID, &a.Created)
}

// gooPayStarted stores what Start returned.
func (s *Server) gooPayStarted(ctx context.Context, a *gooPayAttempt, providerRef string, start []byte) error {
	a.ProviderRef, a.Start = providerRef, start
	_, err := s.store.Pool.Exec(ctx, `UPDATE goo_payment_attempts SET provider_ref = $2, start = $3, updated_at = now() WHERE id = $1`,
		a.ID, providerRef, start)
	return err
}

// gooPayEndAttempt marks an attempt that could not be started failed.
func (s *Server) gooPayEndAttempt(ctx context.Context, a *gooPayAttempt, code, providerCode, providerMsg string) {
	a.Status = "failed"
	_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_payment_attempts SET status = 'failed', code = $2, provider_code = $3, provider_message = $4,
		start = NULL, updated_at = now() WHERE id = $1 AND status = 'started'`, a.ID, code, pay.Trunc(providerCode, 64), pay.Trunc(providerMsg, 255))
}

func (s *Server) gooPayAttemptByRef(ctx context.Context, ref string) (*gooPayAttempt, error) {
	return gooScanPayAttempt(s.store.Pool.QueryRow(ctx, `SELECT `+gooPayAttemptCols+` FROM goo_payment_attempts WHERE ref = $1`, ref))
}

// gooPayDB is what applying a result needs from the database: the pool. No transaction is held while a
// callback is handled, because the provider calls inside it (capture, status query) can take seconds
// and the order functions take their own connections: a transaction around both would pin one
// connection per callback and wait for a second, which stalls the site once the pool is used up.
type gooPayDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// gooPayLocked runs fn with the order's lock held and the attempt as it is stored now, so two callbacks
// of one order never run a provider call or apply at the same time in this process; the second one
// sees what the first stored. Across processes the steps are safe to repeat: the provider calls by
// contract, the order changes through the status machine and the "_goo_pay_ref" marker.
func (s *Server) gooPayLocked(ctx context.Context, a *gooPayAttempt, fn func(ctx context.Context, db gooPayDB, a *gooPayAttempt) error) error {
	return gooWithOrderLock(ctx, a.OrderID, func(ctx context.Context) error {
		fresh, err := gooScanPayAttempt(s.store.Pool.QueryRow(ctx, `SELECT `+gooPayAttemptCols+` FROM goo_payment_attempts WHERE id = $1`, a.ID))
		if err != nil {
			return err
		}
		if err := fn(ctx, s.store.Pool, fresh); err != nil {
			return err
		}
		*a = *fresh
		return nil
	})
}

// Callbacks ----------------------------------------------------------------------------------------------

// HandleAPI is /wc-api/goo_<id>/?pay=go|done|return|fail|notify.
func (g *gooPayGateway) HandleAPI(s *Server, c *gin.Context) {
	gooNoStore(c)
	switch kind := c.Query("pay"); kind {
	case "go", "done":
		a, o := s.gooPayCustomerAttempt(c, g)
		if a == nil {
			s.gooPayBadRequest(c)
			return
		}
		if kind == "done" {
			s.gooPayDone(c, a, o)
		} else if a.Status != "started" {
			c.Redirect(http.StatusSeeOther, s.gooPayURL(c.Request.Context(), g.ID(), "done", a.Ref, o.OrderKey))
		} else {
			s.gooPayGoPage(c, g, a, o)
		}
	case "return", "fail", "notify":
		s.gooPayCallback(c, g, pay.CallbackKind(kind))
	default:
		s.gooPayBadRequest(c)
	}
}

// gooPayBadRequest answers like an unknown gateway on the callback route.
func (s *Server) gooPayBadRequest(c *gin.Context) { c.String(http.StatusBadRequest, "-1") }

// gooPayCustomerAttempt is the attempt and order named by ?a=<ref>&k=<order key>; nil when either is
// wrong (the key is compared in constant time).
func (s *Server) gooPayCustomerAttempt(c *gin.Context, g *gooPayGateway) (*gooPayAttempt, *gooOrder) {
	ctx := c.Request.Context()
	ref := c.Query("a")
	if !gooPayRefOK(ref) {
		return nil, nil
	}
	a, err := s.gooPayAttemptByRef(ctx, ref)
	if err != nil || a.Gateway != g.ID() {
		return nil, nil
	}
	o, err := s.gooGetOrder(ctx, a.OrderID)
	if err != nil || o.Type != "shop_order" || !gooOrderKeyMatches(o.OrderKey, c.Query("k")) {
		return nil, nil
	}
	return a, o
}

// gooPayDone is where the browser lands after a callback, as a same-site GET so the session is there:
// the cart is emptied once the order is paid (or held for review), a failure goes back to the Pay
// for order page with the reason.
func (s *Server) gooPayDone(c *gin.Context, a *gooPayAttempt, o *gooOrder) {
	ctx := c.Request.Context()
	switch {
	case a.Status == "paid" || o.IsPaid() || o.Status == "on-hold":
		s.gooCartFor(c).Empty()
		s.gooRedirect(c, s.gooOrderReceivedURL(ctx, o))
	case a.Status == "failed" || a.Status == "expired":
		s.gooAddNotice(c, "error", gooEsc(s.gooPayT(ctx, pay.Message(pay.Code(a.Code)))))
		s.gooRedirect(c, s.gooOrderPayURL(ctx, o))
	default:
		s.gooAddNotice(c, "notice", gooEsc(s.gooPayT(ctx, "We are waiting for the payment provider to confirm your payment. This page will show the result once it arrives.")))
		s.gooRedirect(c, s.gooOrderReceivedURL(ctx, o))
	}
}

// gooPayInput reads a callback request for the provider. The bridge's routing parameters are not
// part of the query the provider sees.
func gooPayInput(c *gin.Context, kind pay.CallbackKind) (pay.CallbackInput, error) {
	r := c.Request
	in := pay.CallbackInput{Kind: kind, Method: r.Method, Query: url.Values{}, Form: url.Values{}, Header: r.Header.Clone()}
	for k, v := range r.URL.Query() {
		switch k {
		case "pay", "a", "k", "wc-api":
			continue
		}
		in.Query[k] = v
	}
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, pay.MaxBody+1))
		if err != nil || len(body) > pay.MaxBody {
			return in, errors.New("callback body too large")
		}
		in.Body = body
		r.Body = io.NopCloser(bytes.NewReader(body))
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		switch {
		case strings.HasPrefix(ct, "multipart/form-data"):
			if err := r.ParseMultipartForm(pay.MaxBody); err == nil && r.MultipartForm != nil {
				for k, v := range r.MultipartForm.Value {
					in.Form[k] = v
				}
			}
		case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
			if f, err := url.ParseQuery(string(body)); err == nil {
				in.Form = f
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	// A middleware may have parsed (and so drained) a form body before the route ran.
	if len(in.Form) == 0 && len(r.PostForm) > 0 {
		in.Form = r.PostForm
	}
	return in, nil
}

// errGooPayVerify marks a callback the provider refused to verify.
var errGooPayVerify = errors.New("callback not verified")

// gooPayCallback handles return, fail (the customer's browser, often a cross-site POST without the
// session cookie) and notify (the provider's server).
func (s *Server) gooPayCallback(c *gin.Context, g *gooPayGateway, kind pay.CallbackKind) {
	ctx := c.Request.Context()
	in, err := gooPayInput(c, kind)
	if err != nil {
		s.gooPayBadRequest(c)
		return
	}
	// The reference is read before the attempt (and so its mode) is known: try the current mode,
	// then the other one, so a callback of an attempt started before the mode changed still routes.
	test := s.gooGatewayTestMode(g.ID())
	ref := ""
	for _, mode := range []bool{test, !test} {
		if p, err := g.provider(s, mode); err == nil {
			if ref = p.CallbackRef(in); ref != "" {
				break
			}
		}
	}
	if ref == "" {
		ref = c.Query("a")
	}
	if !gooPayRefOK(ref) {
		s.gooPayBadRequest(c)
		return
	}
	a, err := s.gooPayAttemptByRef(ctx, ref)
	if err != nil || a.Gateway != g.ID() {
		s.gooPayBadRequest(c)
		return
	}
	// A browser callback must carry the order key the bridge put into the return URL. Without it the
	// attempt reference alone (which the provider also knows and shows in its panel) could fail an
	// order through a provider whose cancel landing needs no proof, and the redirect below would hand
	// out the order key.
	doneURL := ""
	if kind != pay.CallbackNotify {
		o, err := s.gooGetOrder(ctx, a.OrderID)
		if err != nil || o.Type != "shop_order" || !gooOrderKeyMatches(o.OrderKey, c.Query("k")) {
			s.gooPayBadRequest(c)
			return
		}
		doneURL = s.gooPayURL(ctx, g.ID(), "done", a.Ref, o.OrderKey)
	}
	p, err := g.provider(s, a.Test)
	if err != nil {
		log.Printf("goocommerce: %s: callback for attempt %s: provider not configured: %q", g.ID(), a.Ref, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		s.gooPayBadRequest(c)
		return
	}
	var ack *pay.Ack
	err = s.gooPayLocked(ctx, a, func(ctx context.Context, db gooPayDB, a *gooPayAttempt) error {
		res, err := p.Complete(ctx, in, a.toPay())
		if err != nil || res == nil {
			if err == nil {
				err = errors.New("no result")
			}
			return fmt.Errorf("%w: %w", errGooPayVerify, err)
		}
		ack = res.Ack
		if res.Status == pay.StatusPending && kind != pay.CallbackNotify && a.Status == "started" && g.has(pay.FeatureQuery) {
			if q, err := p.Query(ctx, a.toPay()); err == nil && q != nil {
				res = q
			} else if err != nil {
				log.Printf("goocommerce: %s: query of attempt %s: %q", g.ID(), a.Ref, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
			}
		}
		return s.gooPayApply(ctx, db, g, a, res)
	})
	if errors.Is(err, errGooPayVerify) {
		log.Printf("goocommerce: %s: %s callback for attempt %s refused: %q", g.ID(), kind, a.Ref, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		if kind != pay.CallbackNotify {
			// Nothing changed. The customer (who holds the order key) lands on the result page, which
			// shows the order's real state; a notification or the resolver settles a payment whose
			// outcome could not be read here (a timeout while capturing, for example).
			c.Redirect(http.StatusSeeOther, doneURL)
			return
		}
		s.gooPayBadRequest(c)
		return
	}
	if err != nil {
		log.Printf("goocommerce: %s: %s callback for attempt %s: %v", g.ID(), kind, a.Ref, err)
		if kind == pay.CallbackNotify {
			c.String(http.StatusInternalServerError, "error")
			return
		}
	}
	if kind == pay.CallbackNotify {
		if ack == nil {
			c.String(http.StatusOK, "OK")
			return
		}
		status, ct := ack.Status, ack.ContentType
		if status == 0 {
			status = http.StatusOK
		}
		if ct == "" {
			ct = "text/plain; charset=utf-8"
		}
		c.Data(status, ct, []byte(ack.Body))
		return
	}
	c.Redirect(http.StatusSeeOther, doneURL)
}

// gooPayApply brings the attempt and its order in line with a verified result. It runs under
// gooPayLocked; attempt changes go through db, order changes through the usual order functions. A
// returned error means the outcome was not stored (the provider should retry).
func (s *Server) gooPayApply(ctx context.Context, db gooPayDB, g *gooPayGateway, a *gooPayAttempt, res *pay.Result) error {
	switch res.Status {
	case pay.StatusSuccess:
		return s.gooPayApplySuccess(ctx, db, g, a, res)
	case pay.StatusFailure, pay.StatusCancelled:
		return s.gooPayApplyFailure(ctx, db, g, a, res)
	}
	return nil
}

func (s *Server) gooPayApplySuccess(ctx context.Context, db gooPayDB, g *gooPayGateway, a *gooPayAttempt, res *pay.Result) error {
	if a.Status == "paid" || a.Code == "amount_mismatch" {
		return nil // a repeated or late confirmation: everything was done the first time
	}
	// The provider's count when it reports one, else the count the payment was started with.
	inst := a.Installment
	if res.Installment > 0 {
		inst = res.Installment
	}
	mark := func(status, code string) error {
		_, err := db.Exec(ctx, `UPDATE goo_payment_attempts SET status = $2, txn_id = $3, paid_minor = $4, installment = $5, code = $6,
			start = NULL, updated_at = now() WHERE id = $1`, a.ID, status, pay.Trunc(res.TxnID, 255), res.PaidAmount, inst, code)
		if err == nil {
			a.Status, a.TxnID, a.Paid, a.Installment, a.Code = status, res.TxnID, res.PaidAmount, inst, code
		}
		return err
	}
	o, err := s.gooGetOrder(ctx, a.OrderID)
	if err != nil {
		if gooNotFound(err) {
			log.Printf("goocommerce: %s: attempt %s was paid but its order %d no longer exists; refund it in the provider's panel", g.ID(), a.Ref, a.OrderID)
			return mark("paid", "")
		}
		return err
	}
	title, txn, ref := gooEsc(g.spec.Title), gooEsc(res.TxnID), gooEsc(a.Ref)
	paidBy := gxaStr(o.Meta, "_goo_pay_ref")
	if paidBy == a.Ref {
		// This attempt already moved the order; only its own row was not stored.
		if containsStr(gooNeedsPaymentStatuses, o.Status) {
			if err := s.gooPaymentComplete(ctx, o, res.TxnID); err != nil && !errors.Is(err, errGooStatusChanged) {
				return err
			}
		}
		return mark("paid", "")
	}
	if paidBy != "" || o.IsPaid() || o.Status == "refunded" {
		note := fmt.Sprintf("A second %s payment arrived for attempt %s (transaction %s, %s %s) although the order was already paid. Refund it in the %s panel.",
			title, ref, txn, gooEsc(pay.FormatMinor(res.PaidAmount, a.Currency)), gooEsc(a.Currency), title)
		if err := s.gooAddOrderNote(ctx, o, note, false, ""); err != nil {
			return err
		}
		return mark("paid", "")
	}
	if res.Currency == "" {
		// Some providers' answers carry no currency (a single-currency merchant account, or a verified
		// answer that echoes only the amount). The attempt's own currency is what the provider was
		// asked to charge, so it stands in; logged so a provider that should report one is noticed.
		// Every check below then compares against an explicit currency.
		log.Printf("goocommerce: %s: attempt %s: the provider reported no currency; using the attempt's %s", g.ID(), a.Ref, a.Currency) //nolint:gosec // G706: gateway id, validated attempt reference and currency code
		res.Currency = a.Currency
	}
	if res.PaidAmount >= a.Amount && strings.EqualFold(res.Currency, a.Currency) && gooPayMinor(o.Total, o.Currency) != a.Amount {
		// The customer changed the installment count on the Pay for order page after starting this
		// payment: when the order matches it again with this attempt's count, accept it on that count.
		ok, err := s.gooPayOrderMatchAttempt(ctx, g, o, a)
		if err != nil {
			return err
		}
		if ok {
			if err := s.gooSaveOrder(ctx, o, true); err != nil {
				return err
			}
			note := fmt.Sprintf("The installment surcharge was set back to %d installments: %s payment %s (attempt %s) was started with that count before another one was chosen.",
				a.Installment, title, txn, ref)
			if a.Installment <= 1 {
				note = fmt.Sprintf("The installment surcharge was removed: %s payment %s (attempt %s) was started as a single payment before installments were chosen.", title, txn, ref)
			}
			if err := s.gooAddOrderNote(ctx, o, note, false, ""); err != nil {
				log.Printf("goocommerce: %s: order %d: note: %v", g.ID(), o.ID, err)
			}
		}
	}
	problem := ""
	switch {
	case res.PaidAmount <= 0:
		problem = "the provider did not confirm the amount"
	case res.PaidAmount < a.Amount:
		problem = fmt.Sprintf("received %s %s, the payment was started for %s %s", pay.FormatMinor(res.PaidAmount, a.Currency), a.Currency,
			pay.FormatMinor(a.Amount, a.Currency), a.Currency)
	case !strings.EqualFold(res.Currency, a.Currency):
		problem = fmt.Sprintf("paid in %s, the payment was started in %s", res.Currency, a.Currency)
	case !strings.EqualFold(o.Currency, a.Currency) || gooPayMinor(o.Total, o.Currency) != a.Amount:
		problem = fmt.Sprintf("the order total changed to %s %s after the payment was started for %s %s", pay.FormatMinor(gooPayMinor(o.Total, o.Currency), o.Currency),
			o.Currency, pay.FormatMinor(a.Amount, a.Currency), a.Currency)
	case o.PaymentMethod != g.ID():
		problem = "the order's payment method changed to " + o.PaymentMethod
	}
	if problem != "" {
		// As gooStripeApplyIntent: a payment that does not match the order is not accepted as paid.
		if o.TransactionID == "" {
			o.TransactionID = res.TxnID
		}
		note := fmt.Sprintf("%s payment %s (attempt %s) was not accepted: %s. Review the payment before fulfilling the order, and refund it in the %s panel if needed.",
			title, txn, ref, gooEsc(problem), title)
		if err := s.gooSetStatusFrom(ctx, o, gooNeedsPaymentStatuses, "on-hold", note, ""); err != nil {
			if !errors.Is(err, errGooStatusChanged) {
				return err
			}
			if err := s.gooAddOrderNote(ctx, o, note, false, ""); err != nil {
				return err
			}
		}
		return mark("failed", "amount_mismatch")
	}
	o.Meta["_goo_pay_ref"] = a.Ref
	o.Meta["_goo_pay_paid_minor"] = float64(res.PaidAmount)
	if inst > 0 {
		o.Meta["_goo_pay_installment"] = float64(inst)
	}
	if err := s.gooSaveOrder(ctx, o, false); err != nil {
		return err
	}
	if err := s.gooPaymentComplete(ctx, o, res.TxnID); err != nil && !errors.Is(err, errGooStatusChanged) {
		return err
	}
	note := fmt.Sprintf("%s payment complete (transaction %s, attempt %s).", title, txn, ref)
	if a.Test {
		note += " TEST MODE: no real money was charged."
	}
	if inst > 1 {
		note += fmt.Sprintf(" %d installments.", inst)
	}
	if res.PaidAmount > a.Amount {
		note += fmt.Sprintf(" The customer paid %s %s including installment interest charged by the provider.", gooEsc(pay.FormatMinor(res.PaidAmount, a.Currency)), gooEsc(a.Currency))
	}
	if err := s.gooAddOrderNote(ctx, o, note, false, ""); err != nil {
		log.Printf("goocommerce: %s: order %d: note: %v", g.ID(), o.ID, err)
	}
	return mark("paid", "")
}

func (s *Server) gooPayApplyFailure(ctx context.Context, db gooPayDB, g *gooPayGateway, a *gooPayAttempt, res *pay.Result) error {
	if a.Status != "started" {
		return nil // paid, failed or expired already: a late failure changes nothing
	}
	code := res.Code
	if code == pay.CodeNone {
		code = pay.CodeUnknown
	}
	// The attempt row is written last: when moving the order fails, the attempt stays "started" and
	// the provider's retry (or the customer's next callback) does the whole step again.
	mark := func() error {
		if _, err := db.Exec(ctx, `UPDATE goo_payment_attempts SET status = 'failed', code = $2, provider_code = $3, provider_message = $4,
			start = NULL, updated_at = now() WHERE id = $1 AND status = 'started'`, a.ID, string(code), pay.Trunc(res.ProviderCode, 64), pay.Trunc(res.ProviderMessage, 255)); err != nil {
			return err
		}
		a.Status, a.Code = "failed", string(code)
		return nil
	}
	o, err := s.gooGetOrder(ctx, a.OrderID)
	if err != nil {
		if gooNotFound(err) {
			return mark()
		}
		return err
	}
	// Only the order's newest attempt may fail it: a late failure of an older attempt must not fail
	// an order a newer attempt is paying.
	var newest int64
	if err := db.QueryRow(ctx, `SELECT max(id) FROM goo_payment_attempts WHERE order_id = $1`, a.OrderID).Scan(&newest); err != nil {
		return err
	}
	if newest != a.ID || o.PaymentMethod != g.ID() || gxaStr(o.Meta, "_goo_pay_ref") != "" {
		return mark()
	}
	note := fmt.Sprintf("%s payment failed (attempt %s): %s", gooEsc(g.spec.Title), gooEsc(a.Ref), gooEsc(string(code)))
	if res.ProviderCode != "" || res.ProviderMessage != "" {
		note += " – " + gooEsc(pay.Trunc(strings.TrimSpace(res.ProviderCode+" "+res.ProviderMessage), 200))
	}
	if err := s.gooSetStatusFrom(ctx, o, []string{"pending"}, "failed", note, ""); err != nil && !errors.Is(err, errGooStatusChanged) {
		return err
	}
	return mark()
}

// Refunds ------------------------------------------------------------------------------------------------

func (g *gooPayGateway) ProcessRefund(s *Server, ctx context.Context, o *gooOrder, amount float64, reason string) error {
	if !g.has(pay.FeatureRefunds) {
		return errors.New("Refunds are not supported by this payment method.")
	}
	// The caller (gooCreateRefund) holds the order's lock; taking it here too keeps two refunds of one
	// payment from both reading the same refunded total when called otherwise.
	return gooWithOrderLock(ctx, o.ID, func(ctx context.Context) error { return g.refundLocked(s, ctx, o, amount, reason) })
}

func (g *gooPayGateway) refundLocked(s *Server, ctx context.Context, o *gooOrder, amount float64, reason string) error {
	ref := gxaStr(o.Meta, "_goo_pay_ref")
	a, err := gooScanPayAttempt(s.store.Pool.QueryRow(ctx, `SELECT `+gooPayAttemptCols+` FROM goo_payment_attempts
		WHERE order_id = $1 AND gateway = $2 AND status = 'paid' ORDER BY (ref = $3) DESC, id DESC LIMIT 1`, o.ID, g.ID(), ref))
	if err != nil {
		if gooNotFound(err) {
			return errors.New("This order has no " + g.spec.Title + " payment to refund.")
		}
		return err
	}
	minor := gooPayMinor(amount, o.Currency)
	if minor <= 0 || minor > a.Paid-a.Refunded {
		return fmt.Errorf("The refund amount exceeds what is left of the %s payment.", g.spec.Title)
	}
	p, err := g.provider(s, a.Test)
	if err != nil {
		return fmt.Errorf("%s is not configured: %w", g.spec.Title, err)
	}
	{
		res, err := p.Refund(ctx, pay.RefundRequest{Attempt: a.toPay(), Amount: minor, Full: minor == a.Paid && a.Refunded == 0, // against what was captured (installment interest included), not the order amount
			RefundRef: gooPayRef(), Reason: pay.Trunc(reason, 200)})
		if errors.Is(err, pay.ErrUnknownOutcome) {
			log.Printf("goocommerce: %s: refund of attempt %s: %q", g.ID(), a.Ref, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
			return fmt.Errorf("The outcome of the %s refund is unknown (the provider did not answer in time). Check the payment in the %s panel before trying again.", g.spec.Title, g.spec.Title)
		}
		if err != nil {
			var pe *pay.Error
			if errors.As(err, &pe) {
				return fmt.Errorf("%s refund failed: %s (%s)", g.spec.Title, pay.Message(pe.Code), strings.TrimSpace(pe.ProviderCode+" "+pay.Trunc(pe.ProviderMessage, 200)))
			}
			log.Printf("goocommerce: %s: refund of attempt %s: %q", g.ID(), a.Ref, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
			return fmt.Errorf("%s refund failed.", g.spec.Title)
		}
		if res == nil || res.Status != pay.StatusSuccess {
			detail := ""
			if res != nil {
				detail = strings.TrimSpace(res.ProviderCode + " " + pay.Trunc(res.ProviderMessage, 200))
			}
			return fmt.Errorf("%s refund failed. %s", g.spec.Title, detail)
		}
		if _, err := s.store.Pool.Exec(ctx, `UPDATE goo_payment_attempts SET refunded_minor = refunded_minor + $2, updated_at = now() WHERE id = $1`, a.ID, minor); err != nil {
			log.Printf("goocommerce: %s: refund of attempt %s succeeded but was not recorded on the attempt: %q", g.ID(), a.Ref, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
		}
		_ = s.gooAddOrderNote(ctx, o, fmt.Sprintf("Refunded %s %s through %s (attempt %s, transaction %s).", gooEsc(pay.FormatMinor(minor, a.Currency)),
			gooEsc(a.Currency), gooEsc(g.spec.Title), gooEsc(a.Ref), gooEsc(res.TxnID)), false, "")
		return nil
	}
}

// Pending resolver ---------------------------------------------------------------------------------------

// gooPayResolveBatch bounds the status queries of one run.
const gooPayResolveBatch = 50

// gooPayResolve asks providers with a status API about attempts left open (the customer closed the
// page, a notification was lost) and expires attempts nobody finished within two days.
func (s *Server) gooPayResolve(ctx context.Context) {
	var ids []string
	for id, gw := range gooGatewayRegistry {
		if g, ok := gw.(*gooPayGateway); ok && g.has(pay.FeatureQuery) {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		rows, err := s.store.Pool.Query(ctx, `SELECT `+gooPayAttemptCols+` FROM goo_payment_attempts WHERE status = 'started' AND gateway = ANY($1)
			AND created_at < now() - interval '15 minutes' AND created_at > now() - interval '48 hours' ORDER BY created_at LIMIT $2`, ids, gooPayResolveBatch)
		var open []*gooPayAttempt
		if err == nil {
			for rows.Next() {
				if a, err := gooScanPayAttempt(rows); err == nil {
					open = append(open, a)
				}
			}
			rows.Close()
		}
		for _, a := range open {
			g, _ := gooGatewayRegistry[a.Gateway].(*gooPayGateway)
			if g == nil {
				continue
			}
			p, err := g.provider(s, a.Test)
			if err != nil {
				continue
			}
			err = s.gooPayLocked(ctx, a, func(ctx context.Context, db gooPayDB, a *gooPayAttempt) error {
				if a.Status != "started" {
					return nil
				}
				res, err := p.Query(ctx, a.toPay())
				if err != nil || res == nil {
					return err
				}
				return s.gooPayApply(ctx, db, g, a, res)
			})
			if err != nil && !errors.Is(err, pay.ErrNotSupported) {
				log.Printf("goocommerce: %s: resolving attempt %s: %q", a.Gateway, a.Ref, err.Error()) //nolint:gosec // G706: gateway and order ids, a validated attempt reference and a quoted error text
			}
		}
	}
	_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_payment_attempts SET status = 'expired', start = NULL, updated_at = now()
		WHERE status = 'started' AND created_at < now() - interval '48 hours'`)
}
