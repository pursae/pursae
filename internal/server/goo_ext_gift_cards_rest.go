package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Gift Cards REST: the Store API takes the gift card fields of a product (extensions.gift_cards) and
// applies / removes codes on the cart; wc/v3/gift-cards lists, reads, adjusts and disables cards for
// POS and accounting integrations.

// gcStoreForm maps the Store API add-item fields (extensions.gift_cards, or "gift_card") to the form
// fields gcAddToCart reads: amount, custom_amount, to, to_name, from, message, delivery.
func gcStoreForm(body map[string]any) map[string][]string {
	m := gooObj(body["gift_card"])
	if len(m) == 0 {
		m = gooObj(gooObj(body["extensions"])["gift_cards"])
	}
	if len(m) == 0 {
		return nil
	}
	form := map[string][]string{}
	for _, k := range []string{"amount", "custom_amount", "to", "to_name", "from", "message", "delivery"} {
		if v, ok := m[k]; ok && gooStr(v) != "" {
			form["wc_gc_giftcard_"+k] = []string{gooStr(v)}
		}
	}
	return form
}

// gcStoreExtensions is the cart's extensions.gift_cards: the applied codes with their balances.
func (s *Server) gcStoreExtensions(c *gin.Context, cart *gooCart) map[string]any {
	out := map[string]any{}
	if !s.gooExtOn(gcKey) {
		return out
	}
	applied := []any{}
	for _, code := range gcApplied(cart.sess) {
		if g, err := s.gcByCode(c.Request.Context(), code); err == nil {
			applied = append(applied, map[string]any{"code": g.Code, "balance": s.gooMinor(g.Balance)})
		}
	}
	out["gift_cards"] = map[string]any{"applied": applied}
	return out
}

func (s *Server) gcStoreApply(c *gin.Context, req *restRequest) {
	if !s.gooExtOn(gcKey) {
		gooNoRoute(c)
		return
	}
	cart := s.gooCartFor(c)
	code, _ := req.Str(c, "code")
	if _, err := s.gcApply(c, code); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errGooThrottled) {
			status = http.StatusTooManyRequests
		}
		s.gooStoreCartErr(c, status, "woocommerce_rest_cart_gift_card_error", err.Error(), cart)
		return
	}
	cart.Calculate()
	s.gooStoreCartRespond(c, http.StatusOK, cart)
}

func (s *Server) gcStoreRemove(c *gin.Context, req *restRequest) {
	if !s.gooExtOn(gcKey) {
		gooNoRoute(c)
		return
	}
	cart := s.gooCartFor(c)
	code, _ := req.Str(c, "code")
	code = gcNormalize(code)
	have := gcApplied(cart.sess)
	var keep []string
	for _, h := range have {
		if h != code {
			keep = append(keep, h)
		}
	}
	if len(keep) == len(have) {
		s.gooStoreCartErr(c, http.StatusBadRequest, "woocommerce_rest_cart_gift_card_invalid_code", "Gift card cannot be removed because it is not applied to the cart.", cart)
		return
	}
	gcSetApplied(cart.sess, keep)
	cart.sess.touch()
	cart.Calculate()
	s.gooStoreCartRespond(c, http.StatusOK, cart)
}

func (s *Server) registerGooGiftCardsREST() {
	mw := gooCapsAll("manage_woocommerce")
	on := func(h restHandler) restHandler {
		return func(c *gin.Context, req *restRequest) {
			if !s.gooExtOn(gcKey) {
				gooNoRoute(c)
				return
			}
			h(c, req)
		}
	}
	s.gooV3("/gift-cards", mw, map[string]restHandler{http.MethodGet: on(s.gcV3List)})
	s.gooV3("/gift-cards/(?P<id>[\\d]+)", mw, map[string]restHandler{http.MethodGet: on(s.gcV3Get), http.MethodPut: on(s.gcV3Update)})
	s.gooV3("/gift-cards/(?P<id>[\\d]+)/adjust", mw, map[string]restHandler{http.MethodPost: on(s.gcV3Adjust)})
}

func (s *Server) gcV3JSON(ctx context.Context, g *gcCard, withActivity bool) map[string]any {
	var expires any
	if g.Expires != nil {
		expires = g.Expires.Format("2006-01-02")
	}
	out := map[string]any{"id": g.ID, "code": g.Code, "status": g.Status, "currency": g.Currency, "initial_balance": gooRound(g.Initial, s.gooDecimals()),
		"balance": gooRound(g.Balance, s.gooDecimals()), "order_id": g.OrderID, "recipient": g.Recipient, "recipient_name": g.RecipientName,
		"sender": g.Sender, "message": g.Message, "delivered": g.Delivered, "date_expires": expires, "date_created": s.gooDate(&g.Created)}
	if withActivity {
		acts := []any{}
		for _, a := range s.gcActivities(ctx, g.ID) {
			acts = append(acts, map[string]any{"id": a.ID, "type": a.Type, "amount": a.Amount, "order_id": a.OrderID, "user_id": a.UserID, "note": a.Note, "date": s.gooDate(&a.Date)})
		}
		out["activity"] = acts
	}
	return out
}

func (s *Server) gcV3List(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	where := []string{"true"}
	var args []any
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	switch st := c.Query("status"); st {
	case "active", "disabled", "redeemed":
		where = append(where, "status = "+arg(st))
	case "expired":
		where = append(where, "expires < current_date")
	case "", "any", "all":
	default:
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): status")
		return
	}
	if q := strings.TrimSpace(c.Query("search")); q != "" {
		where = append(where, "(code ILIKE "+arg("%"+q+"%")+" OR recipient ILIKE "+arg("%"+q+"%")+")")
	}
	if v := c.Query("order_id"); v != "" {
		where = append(where, "order_id = "+arg(gooInt(v)))
	}
	cond := strings.Join(where, " AND ")
	page, per, offset := gooPage(c)
	if offset < 0 {
		offset = (page - 1) * per
	}
	var total int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM goo_gift_cards WHERE `+cond, args...).Scan(&total)
	paginate(c, total, per, page)
	order := "DESC"
	if strings.EqualFold(c.Query("order"), "asc") {
		order = "ASC"
	}
	out := []any{}
	for _, g := range s.gcQuery(ctx, cond+" ORDER BY id "+order+" LIMIT "+strconv.Itoa(per)+" OFFSET "+strconv.Itoa(offset), args...) {
		out = append(out, s.gcV3JSON(ctx, g, false))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gcV3Card(c *gin.Context, req *restRequest) *gcCard {
	g, err := s.gcGet(c.Request.Context(), req.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "woocommerce_rest_gift_card_invalid_id", "Invalid ID.")
		return nil
	}
	return g
}

func (s *Server) gcV3Get(c *gin.Context, req *restRequest) {
	if g := s.gcV3Card(c, req); g != nil {
		restJSON(c, http.StatusOK, s.gcV3JSON(c.Request.Context(), g, true))
	}
}

// gcV3Update changes the status (active / disabled) and the expiry date (empty = none) of a card.
func (s *Server) gcV3Update(c *gin.Context, req *restRequest) {
	g := s.gcV3Card(c, req)
	if g == nil {
		return
	}
	ctx := c.Request.Context()
	b := gooFormBody(c, req)
	uid := int64(0)
	if u, _ := s.gooV3User(c, req.Method); u != nil {
		uid = u.ID
	}
	if v, ok := b["date_expires"]; ok {
		if str := gooStr(v); str == "" {
			_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_gift_cards SET expires = NULL WHERE id = $1`, g.ID)
		} else if e, err := time.ParseInLocation("2006-01-02", str, s.gxLoc()); err == nil {
			_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_gift_cards SET expires = $2 WHERE id = $1`, g.ID, e)
		} else {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): date_expires")
			return
		}
	}
	switch st := gooStr(b["status"]); st {
	case "":
	case "disabled":
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_gift_cards SET status = 'disabled' WHERE id = $1`, g.ID)
		_ = s.gcLog(ctx, g.ID, "manual", 0, 0, uid, "Disabled")
	case "active":
		g.Status = "active"
		_ = s.gcAdjust(ctx, g, 0, "manual", 0, uid, "Enabled")
	default:
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): status")
		return
	}
	g, _ = s.gcGet(ctx, g.ID)
	restJSON(c, http.StatusOK, s.gcV3JSON(ctx, g, true))
}

// gcV3Adjust changes the balance by a signed amount (a debit is clamped at zero).
func (s *Server) gcV3Adjust(c *gin.Context, req *restRequest) {
	g := s.gcV3Card(c, req)
	if g == nil {
		return
	}
	ctx := c.Request.Context()
	b := gooFormBody(c, req)
	amount := gxNum(b["amount"])
	if amount == 0 {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): amount")
		return
	}
	uid := int64(0)
	if u, _ := s.gooV3User(c, req.Method); u != nil {
		uid = u.ID
	}
	if err := s.gcAdjust(ctx, g, amount, "manual", 0, uid, strings.TrimSpace(gooStr(b["note"]))); err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_gift_card_error", err.Error())
		return
	}
	g, _ = s.gcGet(ctx, g.ID)
	restJSON(c, http.StatusOK, s.gcV3JSON(ctx, g, true))
}
