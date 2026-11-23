package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

// Stock reservation for unpaid orders (goo_reserved_stock.go): concurrent checkouts for the last units
// cannot both hold them, a held unit cannot be sold to another order, and holds end with the order.

// gooResvRedirect is a payment gateway that sends the customer away to pay (PayPal, a hosted card
// page): the order stays pending after the checkout until the provider reports the payment.
type gooResvRedirect struct{ gooTestGateway }

func (gooResvRedirect) ID() string { return "goo_resv_redirect" }
func (gooResvRedirect) ProcessPayment(s *Server, c *gin.Context, o *gooOrder) (gooPaymentResult, error) {
	return gooPaymentResult{Result: "success", Redirect: "https://pay.example.test/" + o.OrderKey}, nil
}

func gooResvSetup(t *testing.T) *Server {
	t.Helper()
	_, _, srv := gooSite(t)
	gooRegisterGateway(gooResvRedirect{})
	t.Cleanup(func() { delete(gooGatewayRegistry, "goo_resv_redirect") })
	if err := srv.gooSetOpts(context.Background(), map[string]any{"manage_stock": "yes", "hold_stock_minutes": 15,
		"goo_resv_redirect_settings": map[string]any{"enabled": "yes"}}); err != nil {
		t.Fatal(err)
	}
	return srv
}

// gooResvVisitor is a visitor (its own session) with qty of p in the cart.
func gooResvVisitor(t *testing.T, srv *Server, p *gooProduct, qty int) *gin.Context {
	t.Helper()
	c := gooTestCtx()
	cart := srv.gooCartFor(c)
	cart.sess.Data.Customer.Billing = map[string]string{"country": "US", "state": "CA", "postcode": "90210", "email": "buyer@example.test"}
	if _, err := cart.Add(context.Background(), p.ID, qty, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return c
}

func gooResvHeld(t *testing.T, srv *Server, productID int64) map[int64]int {
	t.Helper()
	rows, err := srv.store.Pool.Query(context.Background(), `SELECT order_id, stock_quantity FROM goo_reserved_stock WHERE product_id = $1 AND expires_at > now()`, productID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		out[id] = n
	}
	return out
}

// Two customers check out the last unit at the same moment and both go to an external payment page:
// only one of them holds it, the other is told it is out of stock before paying.
func TestGooReserveStockLastUnitTwoCheckouts(t *testing.T) {
	srv := gooResvSetup(t)
	ctx := context.Background()
	p := gooMakeSimple(t, srv, "Last One", 10, func(p *gooProduct) { p.Virtual, p.ManageStock, p.StockQuantity = true, true, gooIntPtr(1) })

	for round := range 5 { // the race is real: repeat it so both orders of arrival are exercised
		if _, err := srv.store.Pool.Exec(ctx, `DELETE FROM goo_reserved_stock`); err != nil {
			t.Fatal(err)
		}
		c1, c2 := gooResvVisitor(t, srv, p, 1), gooResvVisitor(t, srv, p, 1)
		var mu sync.Mutex
		var placed []*gooOrder
		var refused []string
		run := func(c *gin.Context) func() {
			return func() {
				o, errs := gxCheckout(t, srv, c, "goo_resv_redirect")
				mu.Lock()
				defer mu.Unlock()
				if len(errs) == 0 {
					placed = append(placed, o)
				} else {
					refused = append(refused, strings.Join(errs, "; "))
				}
			}
		}
		gooRace(run(c1), run(c2))
		if len(placed) != 1 || len(refused) != 1 {
			t.Fatalf("round %d: %d checkouts went to payment, refused: %v", round, len(placed), refused)
		}
		if !strings.Contains(refused[0], "Last One") || !strings.Contains(refused[0], "(0 available)") {
			t.Errorf("round %d: the second checkout was refused with %q", round, refused[0])
		}
		if held := gooResvHeld(t, srv, p.ID); len(held) != 1 || held[placed[0].ID] != 1 {
			t.Errorf("round %d: reservations %v, want order %d holding 1", round, held, placed[0].ID)
		}
		if qty, _ := gooStockOf(t, srv, p.ID); qty != 1 {
			t.Errorf("round %d: stock %d before any payment, want 1", round, qty)
		}
		// Abandoned: the next round starts from a free unit again.
		if err := srv.gooSetStatus(ctx, placed[0], "cancelled", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	// The paying customer gets the unit; the hold ends with the payment.
	c := gooResvVisitor(t, srv, p, 1)
	o, errs := gxCheckout(t, srv, c, "goo_resv_redirect")
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if _, errs := gxCheckout(t, srv, gooResvVisitor(t, srv, p, 1), "goo_resv_redirect"); len(errs) == 0 || !strings.Contains(errs[0], "(0 available)") {
		t.Errorf("a later checkout while the unit is held: %v", errs)
	}
	if err := srv.gooPaymentComplete(ctx, o, "tx-1"); err != nil {
		t.Fatal(err)
	}
	got, _ := srv.gooGetOrder(ctx, o.ID)
	if qty, status := gooStockOf(t, srv, p.ID); got.Status != "processing" || qty != 0 || status != "outofstock" {
		t.Errorf("after payment: order %s, stock %d %s", got.Status, qty, status)
	}
	if held := gooResvHeld(t, srv, p.ID); len(held) != 0 {
		t.Errorf("reservations left after the payment: %v", held)
	}
}

// Many orders reserving a few units at once: exactly as many hold a unit as there are units.
func TestGooReserveStockConcurrentOrders(t *testing.T) {
	srv := gooResvSetup(t)
	ctx := context.Background()
	p := gooMakeSimple(t, srv, "Scarce", 10, func(p *gooProduct) { p.ManageStock, p.StockQuantity = true, gooIntPtr(3) })
	var orders []*gooOrder
	for range 12 {
		orders = append(orders, gooRaceOrder(t, srv, "r@example.test", 0, map[*gooProduct]int{p: 1}))
	}
	var mu sync.Mutex
	ok, short := 0, 0
	var fns []func()
	for _, o := range orders {
		fns = append(fns, func() {
			err := srv.gooReserveStock(ctx, o)
			var sh *gooStockShortage
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.As(err, &sh):
				short++
			default:
				t.Error(err)
			}
		})
	}
	gooRace(fns...)
	if ok != 3 || short != 9 {
		t.Errorf("%d orders reserved, %d refused; want 3 and 9", ok, short)
	}
	total := 0
	for _, n := range gooResvHeld(t, srv, p.ID) {
		total += n
	}
	if total != 3 {
		t.Errorf("%d units reserved of 3", total)
	}
}

// A unit held by one checkout is not sold to another order's payment: that order goes on hold, and the
// holder can still pay.
func TestGooReservedStockBlocksOtherPayment(t *testing.T) {
	srv := gooResvSetup(t)
	ctx := context.Background()
	p := gooMakeSimple(t, srv, "Held", 10, func(p *gooProduct) { p.ManageStock, p.StockQuantity = true, gooIntPtr(1) })
	holder := gooRaceOrder(t, srv, "a@example.test", 0, map[*gooProduct]int{p: 1})
	other := gooRaceOrder(t, srv, "b@example.test", 0, map[*gooProduct]int{p: 1})
	if err := srv.gooReserveStock(ctx, holder); err != nil {
		t.Fatal(err)
	}
	if err := srv.gooPaymentComplete(ctx, other, "tx-other"); err != nil {
		t.Fatal(err)
	}
	if got, _ := srv.gooGetOrder(ctx, other.ID); got.Status != "on-hold" || got.StockReduced {
		t.Errorf("payment for a held unit: %s reduced=%v", got.Status, got.StockReduced)
	}
	if qty, _ := gooStockOf(t, srv, p.ID); qty != 1 {
		t.Errorf("stock %d after the refused payment, want 1", qty)
	}
	if err := srv.gooPaymentComplete(ctx, holder, "tx-holder"); err != nil {
		t.Fatal(err)
	}
	if got, _ := srv.gooGetOrder(ctx, holder.ID); got.Status != "processing" || !got.StockReduced {
		t.Errorf("the holder's payment: %s reduced=%v", got.Status, got.StockReduced)
	}
	if qty, _ := gooStockOf(t, srv, p.ID); qty != 0 {
		t.Errorf("stock %d after the holder paid, want 0", qty)
	}
}

// Holds end: when the order is cancelled or fails, when they expire (the hourly job deletes them), and
// none are made when "Hold stock" is off.
func TestGooReservedStockReleaseAndExpiry(t *testing.T) {
	srv := gooResvSetup(t)
	ctx := context.Background()
	p := gooMakeSimple(t, srv, "Returns", 10, func(p *gooProduct) { p.ManageStock, p.StockQuantity = true, gooIntPtr(1) })
	a := gooRaceOrder(t, srv, "a@example.test", 0, map[*gooProduct]int{p: 1})
	b := gooRaceOrder(t, srv, "b@example.test", 0, map[*gooProduct]int{p: 1})
	c := gooRaceOrder(t, srv, "c@example.test", 0, map[*gooProduct]int{p: 1})
	if err := srv.gooReserveStock(ctx, a); err != nil {
		t.Fatal(err)
	}
	var sh *gooStockShortage
	if err := srv.gooReserveStock(ctx, b); !errors.As(err, &sh) {
		t.Fatalf("second hold of the last unit: %v", err)
	}
	if err := srv.gooReserveStock(ctx, a); err != nil { // the holder renews its own hold (Pay for order)
		t.Errorf("renewing the order's own hold: %v", err)
	}
	if err := srv.gooSetStatus(ctx, a, "failed", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := srv.gooReserveStock(ctx, b); err != nil {
		t.Fatalf("hold after the holder failed: %v", err)
	}
	// Expired: counts for nothing, and the hourly job deletes it.
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE goo_reserved_stock SET expires_at = now() - interval '1 minute' WHERE order_id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.gooReserveStock(ctx, c); err != nil {
		t.Fatalf("hold after the other expired: %v", err)
	}
	if err := srv.gooHourly(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM goo_reserved_stock WHERE order_id = $1`, b.ID).Scan(&n)
	if n != 0 {
		t.Errorf("the expired hold was not deleted")
	}
	if held := gooResvHeld(t, srv, p.ID); len(held) != 1 || held[c.ID] != 1 {
		t.Errorf("reservations %v, want order %d holding 1", held, c.ID)
	}
	// Deleting the order deletes its hold.
	if _, err := srv.store.Pool.Exec(ctx, `DELETE FROM goo_orders WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if held := gooResvHeld(t, srv, p.ID); len(held) != 0 {
		t.Errorf("reservations of a deleted order: %v", held)
	}
	// Hold stock off: nothing is reserved.
	if err := srv.gooSetOpts(ctx, map[string]any{"hold_stock_minutes": 0}); err != nil {
		t.Fatal(err)
	}
	if err := srv.gooReserveStock(ctx, b); err != nil {
		t.Fatal(err)
	}
	if held := gooResvHeld(t, srv, p.ID); len(held) != 0 {
		t.Errorf("reservations with Hold stock off: %v", held)
	}
}

// A customer who leaves the external payment page (back button) and checks out again is not refused
// the last unit because of the hold of their own abandoned order: the new order takes it over.
func TestGooReserveStockSameVisitorRetry(t *testing.T) {
	srv := gooResvSetup(t)
	p := gooMakeSimple(t, srv, "Only One", 10, func(p *gooProduct) { p.Virtual, p.ManageStock, p.StockQuantity = true, true, gooIntPtr(1) })
	c := gooResvVisitor(t, srv, p, 1)
	first, errs := gxCheckout(t, srv, c, "goo_resv_redirect")
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	second, errs := gxCheckout(t, srv, c, "goo_resv_redirect")
	if len(errs) > 0 {
		t.Fatalf("the same visitor's retry was refused: %v", errs)
	}
	if held := gooResvHeld(t, srv, p.ID); len(held) != 1 || held[second.ID] != 1 {
		t.Errorf("reservations %v, want only order %d (first %d) holding 1", held, second.ID, first.ID)
	}
	// Another visitor still cannot take the held unit.
	if _, errs := gxCheckout(t, srv, gooResvVisitor(t, srv, p, 1), "goo_resv_redirect"); len(errs) == 0 || !strings.Contains(errs[0], "(0 available)") {
		t.Errorf("another visitor while the unit is held: %v", errs)
	}
}
