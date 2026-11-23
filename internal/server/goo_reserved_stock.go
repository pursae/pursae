package server

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Stock reservation for unpaid orders (WooCommerce's Automattic\WooCommerce\Checkout\Helpers\ReserveStock,
// wc_reserve_stock_for_order / wc_release_stock_for_order and the wc_reserved_stock table). Stock is
// reduced only when an order is paid (or put on hold), so without a reservation two customers can both
// reach a payment page for the last unit and both pay. Placing an order therefore reserves its
// quantities in goo_reserved_stock for "Hold stock (minutes)":
//
//   - gooReserveStock locks the stock owners' goo_products rows (FOR UPDATE, in id order so two orders
//     cannot deadlock), sums what other orders hold and refuses the order when the rest is not enough.
//     The row lock serializes every reservation and strict reduction of a product, so the check and
//     the insert cannot interleave with another checkout's.
//   - Holds count until expires_at; gooHeldStock ignores expired rows, the hourly job deletes them.
//   - A strict stock reduction (gooStockDelta) leaves other orders' holds alone: a payment can take
//     only stock that is neither sold nor reserved by another order.
//   - The hold is released (gooReleaseStock) once the order leaves pending: paid or on hold (the stock
//     is reduced instead), cancelled or failed (the stock was never taken). Deleting the order deletes
//     its rows (ON DELETE CASCADE).
//
// Products that do not manage stock or allow backorders are never reserved.

// gooReserveMinutes is how long a reservation lasts (woocommerce_hold_stock_minutes); 0 disables it.
func (s *Server) gooReserveMinutes() int {
	if !s.gooManageStock() {
		return 0
	}
	return max(s.gooOptInt("hold_stock_minutes"), 0)
}

// gooReserveStock is wc_reserve_stock_for_order(): it (re)places the order's reservations in one
// transaction. When a product does not have enough unreserved stock nothing changes and a
// *gooStockShortage is returned.
func (s *Server) gooReserveStock(ctx context.Context, o *gooOrder) error {
	mins := s.gooReserveMinutes()
	if mins == 0 || o == nil || o.ID == 0 {
		return nil
	}
	need := map[int64]int{}            // quantity per stock-owning row
	product := map[int64]*gooProduct{} // a product of the line, for the shortage message
	for _, it := range o.ItemsOf("line_item") {
		if red, _ := it.Meta["_reduced_stock"].(float64); red > 0 || it.Qty() <= 0 {
			continue // taken from stock already
		}
		pid := it.ProductID
		if it.VariationID != 0 {
			pid = it.VariationID
		}
		p, err := s.gooGetProduct(ctx, pid)
		if err != nil || !s.gooManagingStock(ctx, p) {
			continue
		}
		owner := s.gooStockWriteOwner(ctx, p)
		need[owner.ID] += it.Qty()
		product[owner.ID] = p
	}
	ids := make([]int64, 0, len(need))
	for id := range need {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return pgx.BeginFunc(ctx, s.store.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM goo_reserved_stock WHERE order_id = $1`, o.ID); err != nil {
			return err
		}
		for _, id := range ids {
			var qty *int
			var manage bool
			var backorders string
			err := tx.QueryRow(ctx, `SELECT stock_quantity, manage_stock, backorders FROM goo_products WHERE post_id = $1 FOR UPDATE`, id).
				Scan(&qty, &manage, &backorders)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if !manage || backorders != "no" {
				continue
			}
			// A new statement after the lock: its snapshot sees every hold committed before it.
			var held int
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(stock_quantity), 0) FROM goo_reserved_stock
				WHERE product_id = $1 AND order_id <> $2 AND expires_at > now()`, id, o.ID).Scan(&held); err != nil {
				return err
			}
			avail := -held
			if qty != nil {
				avail += *qty
			}
			if avail < need[id] {
				return &gooStockShortage{Name: s.gooProductTitle(ctx, product[id]), Available: avail}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO goo_reserved_stock (order_id, product_id, stock_quantity, expires_at)
				VALUES ($1, $2, $3, now() + make_interval(mins => $4))`, o.ID, id, need[id], mins); err != nil {
				return err
			}
		}
		return nil
	})
}

// gooReleaseStock is wc_release_stock_for_order(): the order holds nothing any more.
func (s *Server) gooReleaseStock(ctx context.Context, orderID int64) {
	if orderID == 0 {
		return
	}
	if _, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_reserved_stock WHERE order_id = $1`, orderID); err != nil {
		slog.Warn("goocommerce: release reserved stock", "order", orderID, "err", err)
	}
}

// gooHeldStock is wc_get_held_stock_quantity(): the units of the product's stock reserved by unpaid
// orders other than those of mine (the visitor's own orders, see gooVisitorOrders).
func (s *Server) gooHeldStock(ctx context.Context, p *gooProduct, mine gooOwnOrders) int {
	if s.gooReserveMinutes() == 0 {
		return 0
	}
	var held int
	_ = s.store.Pool.QueryRow(ctx, `SELECT coalesce(sum(r.stock_quantity), 0) FROM goo_reserved_stock r
		JOIN goo_orders o ON o.id = r.order_id
		WHERE r.product_id = $1 AND r.expires_at > now() AND r.order_id <> ALL($2) AND ($3 = 0 OR o.customer_id <> $3)`,
		s.gooStockWriteOwner(ctx, p).ID, mine.IDs, mine.Customer).Scan(&held)
	return held
}

// gooOwnOrders are the orders of one visitor: those its session placed (the order awaiting payment
// and the guest orders) and, signed in, the customer's.
type gooOwnOrders struct {
	IDs      []int64
	Customer int64
}

// gooVisitorOrders is the visitor's own orders. A customer who leaves an external payment page and
// checks out again must not be refused by the hold of their own abandoned order.
func gooVisitorOrders(sess *gooSession, customer int64) gooOwnOrders {
	own := gooOwnOrders{IDs: []int64{}, Customer: customer}
	if sess != nil {
		if sess.Data.OrderAwaitingPayment != 0 {
			own.IDs = append(own.IDs, sess.Data.OrderAwaitingPayment)
		}
		raw, _ := sess.Data.Extra["guest_orders"].([]any)
		for _, v := range raw {
			if id := gooInt(v); id != 0 {
				own.IDs = append(own.IDs, id)
			}
		}
	}
	return own
}

// gooReleaseOwnHolds drops the holds of the visitor's other unpaid orders when keep (a new attempt)
// is placed: the new order takes the units over (they stay pending; paying one later reduces stock
// only from what is free).
func (s *Server) gooReleaseOwnHolds(ctx context.Context, mine gooOwnOrders, keep int64) {
	if len(mine.IDs) == 0 && mine.Customer == 0 {
		return
	}
	if _, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_reserved_stock r USING goo_orders o
		WHERE o.id = r.order_id AND r.order_id <> $1 AND (r.order_id = ANY($2) OR ($3 <> 0 AND o.customer_id = $3))`,
		keep, mine.IDs, mine.Customer); err != nil {
		slog.Warn("goocommerce: release the visitor's earlier holds", "order", keep, "err", err)
	}
}

// gooCleanupReservedStock deletes expired reservations (they count for nothing already).
func (s *Server) gooCleanupReservedStock(ctx context.Context) {
	if _, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_reserved_stock WHERE expires_at <= now()`); err != nil {
		slog.Warn("goocommerce: expired reserved stock", "err", err)
	}
}
