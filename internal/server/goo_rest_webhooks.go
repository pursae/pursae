package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Webhooks (WC_Webhook): the active rows of goo_webhooks are delivered when their topic happens —
// order.created / updated / deleted, product.created / updated / deleted, customer.created / updated /
// deleted, coupon.created / updated / deleted. The payload is the wc/v3 resource; the request carries
// WooCommerce's X-WC-Webhook-* headers with a base64 HMAC-SHA256 signature of the body made with the
// webhook's secret. Deliveries run in the background on a small bounded pool of workers (not one
// goroutine per delivery): a delivery that fails is retried after a pause, with the same delivery id
// so the receiver can drop a duplicate; a delivery that still fails counts as one failure, and a
// webhook is disabled after 5 consecutive failed deliveries (WC_Webhook::failed_delivery()).

const gooWebhookMaxFailures = 5

// gooWebhookWorkers deliver at most this many webhooks at once; gooWebhookQueueSize deliveries wait.
const (
	gooWebhookWorkers   = 4
	gooWebhookQueueSize = 1024
)

// gooWebhookRetryDelays are the pauses before a failed delivery is tried again (tests shorten them).
var gooWebhookRetryDelays = []time.Duration{10 * time.Second, time.Minute}

// gooWebhookTask is one delivery of a payload to one webhook.
type gooWebhookTask struct {
	s          *Server
	h          gooWebhookRow
	source     string
	resourceID int64
	body       []byte
	deliveryID string
	attempt    int
}

var (
	gooWebhookQueue     = make(chan *gooWebhookTask, gooWebhookQueueSize)
	gooWebhookStartOnce sync.Once
)

// gooWebhookEnqueue hands a delivery to the workers; a full queue drops it as a failed delivery.
func gooWebhookEnqueue(t *gooWebhookTask) {
	gooWebhookStartOnce.Do(func() {
		for range gooWebhookWorkers {
			safeGo("goo_rest_webhooks.gooWebhookEnqueue", func() {
				for t := range gooWebhookQueue {
					t.run()
				}
			})
		}
	})
	select {
	case gooWebhookQueue <- t:
	default:
		log.Printf("goocommerce: webhook %d (%s): delivery queue full, delivery dropped", t.h.ID, t.h.Topic)
		t.s.gooWebhookResult(context.Background(), t.h, false)
		gooWebhookWG.Done()
	}
}

// run makes one attempt; a failure is queued again after its pause until the retries run out.
func (t *gooWebhookTask) run() {
	ok, retryable := t.s.gooWebhookDeliver(context.Background(), t.h, t.source, t.resourceID, t.body, t.deliveryID)
	if !ok && retryable && t.attempt < len(gooWebhookRetryDelays) {
		d := gooWebhookRetryDelays[t.attempt]
		t.attempt++
		time.AfterFunc(d, func() {
			select {
			case gooWebhookQueue <- t:
			default:
				t.s.gooWebhookResult(context.Background(), t.h, false)
				gooWebhookWG.Done()
			}
		})
		return
	}
	t.s.gooWebhookResult(context.Background(), t.h, ok)
	gooWebhookWG.Done()
}

// gooWebhookClient delivers webhooks (a variable so tests can swap it). Like wp_safe_remote_request,
// which WooCommerce delivers with, it connects to public addresses only: the address is checked when
// the connection is made, after DNS resolution and on every redirect, so a delivery URL cannot reach
// the server's own services, the private network or a cloud metadata endpoint.
var gooWebhookClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{TLSHandshakeTimeout: 10 * time.Second, DialContext: (&net.Dialer{Timeout: 10 * time.Second,
		Control: publicOnly(gooWebhookAllowPrivate.Load, "webhook delivery URL points at a private network address")}).DialContext},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("unsupported redirect scheme")
		}
		return nil
	},
}

// gooWebhookAllowPrivate lets tests deliver to httptest servers on the loopback address.
var gooWebhookAllowPrivate atomic.Bool

// gooWebhookWG tracks deliveries in flight (tests wait on it).
var gooWebhookWG sync.WaitGroup

func init() {
	// Module topics: booking / membership .created and .updated (subscription.* is fired by gsubWebhook).
	gooOn("booking_created", 50, func(s *Server, ctx context.Context, arg any) {
		if b, ok := arg.(*bkBooking); ok && s.gooWebhooksFor(ctx, "booking.created") {
			s.gooWebhookFire(ctx, "booking.created", b.ID, s.gooV3BookingJSON(ctx, b))
		}
	})
	gooOn("booking_updated", 50, func(s *Server, ctx context.Context, arg any) {
		if b, ok := arg.(*bkBooking); ok && s.gooWebhooksFor(ctx, "booking.updated") {
			s.gooWebhookFire(ctx, "booking.updated", b.ID, s.gooV3BookingJSON(ctx, b))
		}
	})
	gooOn("membership_created", 50, func(s *Server, ctx context.Context, arg any) {
		if m, ok := arg.(*gooUserMembership); ok && s.gooWebhooksFor(ctx, "membership.created") {
			s.gooWebhookFire(ctx, "membership.created", m.ID, s.gooV3MembershipJSON(ctx, m))
		}
	})
	gooOn("membership_updated", 50, func(s *Server, ctx context.Context, arg any) {
		if m, ok := arg.(*gooUserMembership); ok && s.gooWebhooksFor(ctx, "membership.updated") {
			s.gooWebhookFire(ctx, "membership.updated", m.ID, s.gooV3MembershipJSON(ctx, m))
		}
	})
	gooOn("order_created", 50, func(s *Server, ctx context.Context, arg any) {
		if o, ok := arg.(*gooOrder); ok {
			s.gooWebhookOrder(ctx, "order.created", o)
		}
	})
	gooOn("order_status_changed", 50, func(s *Server, ctx context.Context, arg any) {
		if ev, ok := arg.(*gooStatusChange); ok {
			s.gooWebhookOrder(ctx, "order.updated", ev.Order)
		}
	})
	gooOn("order_partially_refunded", 50, func(s *Server, ctx context.Context, arg any) {
		if ev, ok := arg.(*gooRefundEvent); ok {
			s.gooWebhookOrder(ctx, "order.updated", ev.Order)
		}
	})
	gooOn("product_saved", 50, func(s *Server, ctx context.Context, arg any) {
		p, ok := arg.(*gooProduct)
		if !ok || !s.gooWebhooksFor(ctx, "product.created", "product.updated") {
			return
		}
		topic := "product.updated"
		var created, modified time.Time
		if s.store.Pool.QueryRow(ctx, `SELECT created_at, modified_at FROM posts WHERE id = $1`, p.ID).Scan(&created, &modified) == nil && modified.Sub(created) < time.Millisecond {
			topic = "product.created"
		}
		fresh, err := s.gooGetProduct(ctx, p.ID)
		if err != nil {
			return
		}
		payload := s.gooV3ProductJSON(ctx, fresh)
		if fresh.Type == gooTypeVariation {
			payload = s.gooV3VariationJSON(ctx, fresh)
		}
		s.gooWebhookFire(ctx, topic, p.ID, payload)
	})
	gooOn("customer_created", 50, func(s *Server, ctx context.Context, arg any) {
		var id int64
		switch v := arg.(type) {
		case *gooNewCustomer:
			id = v.UserID
		case int64:
			id = v
		}
		if id == 0 || !s.gooWebhooksFor(ctx, "customer.created") {
			return
		}
		if u, err := s.store.UserByID(ctx, id); err == nil {
			s.gooWebhookFire(ctx, "customer.created", id, s.gooV3CustomerJSON(ctx, u))
		}
	})
}

// gooWebhookRow is an active webhook.
type gooWebhookRow struct {
	ID         int64
	Topic, URL string
	Secret     string
	UserID     int64
	Failures   int
}

// gooWebhooksFor reports whether any active webhook listens to one of the topics.
func (s *Server) gooWebhooksFor(ctx context.Context, topics ...string) bool {
	if !s.gooOn() {
		return false
	}
	var ok bool
	_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM goo_webhooks WHERE status = 'active' AND topic = ANY($1))`, topics).Scan(&ok)
	return ok
}

// gooWebhookOrder delivers an order topic with the order's v3 representation.
func (s *Server) gooWebhookOrder(ctx context.Context, topic string, o *gooOrder) {
	if o == nil || o.Type != "shop_order" || !s.gooWebhooksFor(ctx, topic) {
		return
	}
	fresh, err := s.gooGetOrder(ctx, o.ID)
	if err != nil {
		return
	}
	s.gooWebhookFire(ctx, topic, o.ID, s.gooV3OrderJSON(ctx, fresh))
}

// gooWebhookCoupon delivers a coupon topic with the coupon's wc/v3 resource.
func (s *Server) gooWebhookCoupon(ctx context.Context, topic string, id int64) {
	if !s.gooWebhooksFor(ctx, topic) {
		return
	}
	if cp, err := s.gooCouponByID(ctx, id); err == nil {
		s.gooWebhookFire(ctx, topic, id, s.gooV3CouponJSON(ctx, cp))
	}
}

// gooWebhookProduct delivers a product topic with the product's wc/v3 resource.
func (s *Server) gooWebhookProduct(ctx context.Context, topic string, id int64) {
	if !s.gooWebhooksFor(ctx, topic) {
		return
	}
	if p, err := s.gooGetProduct(ctx, id); err == nil {
		s.gooWebhookFire(ctx, topic, id, s.gooV3ProductJSON(ctx, p))
	}
}

// gooWebhookFire delivers a topic to every active webhook subscribed to it, in the background.
func (s *Server) gooWebhookFire(ctx context.Context, topic string, resourceID int64, payload any) {
	s.gooWebhookFireAny(ctx, []string{topic}, resourceID, payload)
}

// gooWebhookFireAny delivers a payload to every active webhook subscribed to one of the topics.
func (s *Server) gooWebhookFireAny(ctx context.Context, topics []string, resourceID int64, payload any) {
	if !s.gooOn() {
		return
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT id, topic, delivery_url, secret, user_id, failures FROM goo_webhooks WHERE status = 'active' AND topic = ANY($1)`, topics)
	if err != nil {
		return
	}
	var hooks []gooWebhookRow
	for rows.Next() {
		var h gooWebhookRow
		if rows.Scan(&h.ID, &h.Topic, &h.URL, &h.Secret, &h.UserID, &h.Failures) == nil {
			h.Secret = s.revealSecret(h.Secret) // stored sealed (secret_seal.go)
			hooks = append(hooks, h)
		}
	}
	rows.Close()
	if len(hooks) == 0 {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	source := strings.TrimRight(s.links(ctx).Home, "/") + "/"
	for _, h := range hooks {
		gooWebhookWG.Add(1)
		gooWebhookEnqueue(&gooWebhookTask{s: s, h: h, source: source, resourceID: resourceID, body: body, deliveryID: gooRandomKey(32)})
	}
}

// gooWebhookAction delivers an internal action (gooFire) to the "action.<name>" webhooks. The topic
// event is the hook name, with or without WooCommerce's "woocommerce_" prefix; the body is
// WC_Webhook::build_payload()'s {"action": hook, "arg": first argument id}.
func (s *Server) gooWebhookAction(ctx context.Context, name string, arg any) {
	topics := []string{"action." + name, "action.woocommerce_" + name}
	if !s.gooWebhooksFor(ctx, topics...) {
		return
	}
	var id int64
	switch v := arg.(type) {
	case *gooOrder:
		if v != nil {
			id = v.ID
		}
	case *gooProduct:
		if v != nil {
			id = v.ID
		}
	case *gooStatusChange:
		if v != nil && v.Order != nil {
			id = v.Order.ID
		}
	case *gooRefundEvent:
		if v != nil && v.Order != nil {
			id = v.Order.ID
		}
	case int64:
		id = v
	}
	var a any
	if id != 0 {
		a = id
	}
	s.gooWebhookFireAny(ctx, topics, id, map[string]any{"action": name, "arg": a})
}

// gooWebhookSignature is WC_Webhook::generate_signature(): base64(HMAC-SHA256(body, secret)).
func gooWebhookSignature(body []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// gooWebhookDeliver posts one attempt of a delivery and logs it. retryable is false when trying again
// cannot help (the URL is malformed or points at a refused address).
func (s *Server) gooWebhookDeliver(ctx context.Context, h gooWebhookRow, source string, resourceID int64, body []byte, deliveryID string) (ok, retryable bool) {
	ok, retryable, _ = s.gooWebhookAttempt(ctx, h, source, body, deliveryID)
	return ok, retryable
}

// gooWebhookAttempt makes the request and records it in goo_webhook_deliveries (time, duration, HTTP
// status, a shortened response body or the transport error).
func (s *Server) gooWebhookAttempt(ctx context.Context, h gooWebhookRow, source string, body []byte, deliveryID string) (ok, retryable bool, rec gooWebhookDelivery) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rec = gooWebhookDelivery{WebhookID: h.ID, DeliveryID: deliveryID, Topic: h.Topic, URL: h.URL}
	defer func() {
		rec.Success = ok
		s.gooWebhookLog(context.Background(), &rec)
	}()
	resource, event, _ := strings.Cut(h.Topic, ".")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		rec.Error = err.Error()
		return false, false, rec
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Goocommerce/"+gooVersion+" Hookshot (Pursae)")
	req.Header.Set("X-WC-Webhook-Source", source)
	req.Header.Set("X-WC-Webhook-Topic", h.Topic)
	req.Header.Set("X-WC-Webhook-Resource", resource)
	req.Header.Set("X-WC-Webhook-Event", event)
	req.Header.Set("X-WC-Webhook-Signature", gooWebhookSignature(body, h.Secret))
	req.Header.Set("X-WC-Webhook-ID", strconv.FormatInt(h.ID, 10))
	req.Header.Set("X-WC-Webhook-Delivery-ID", deliveryID)
	start := time.Now()
	resp, err := gooWebhookClient.Do(req)
	rec.DurationMS = int(time.Since(start).Milliseconds())
	if err != nil {
		rec.Error = err.Error()
		return false, !strings.Contains(err.Error(), "private network address"), rec
	}
	rec.StatusCode = resp.StatusCode
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	rec.Response = strings.ToValidUTF8(string(snippet), "")
	return resp.StatusCode >= 200 && resp.StatusCode < 300, true, rec
}

// gooWebhookDelivery is one logged delivery attempt.
type gooWebhookDelivery struct {
	ID         int64
	WebhookID  int64
	DeliveryID string
	Topic, URL string
	StatusCode int
	DurationMS int
	Response   string
	Error      string
	Success    bool
	Created    time.Time
}

// gooWebhookKeepDeliveries is how many deliveries a webhook's log keeps.
const gooWebhookKeepDeliveries = 50

func (s *Server) gooWebhookLog(ctx context.Context, d *gooWebhookDelivery) {
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO goo_webhook_deliveries (webhook_id, delivery_id, topic, url, status_code, duration_ms, response, error, success)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, d.WebhookID, d.DeliveryID, d.Topic, d.URL, d.StatusCode, d.DurationMS, d.Response, d.Error, d.Success); err != nil {
		return
	}
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_webhook_deliveries WHERE webhook_id = $1 AND id NOT IN
		(SELECT id FROM goo_webhook_deliveries WHERE webhook_id = $1 ORDER BY id DESC LIMIT $2)`, d.WebhookID, gooWebhookKeepDeliveries)
}

// gooWebhookDeliveries lists a webhook's logged deliveries, newest first.
func (s *Server) gooWebhookDeliveries(ctx context.Context, webhookID int64, limit int) []gooWebhookDelivery {
	rows, err := s.store.Pool.Query(ctx, `SELECT id, webhook_id, delivery_id, topic, url, status_code, duration_ms, response, error, success, created_at
		FROM goo_webhook_deliveries WHERE webhook_id = $1 ORDER BY id DESC LIMIT $2`, webhookID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []gooWebhookDelivery
	for rows.Next() {
		var d gooWebhookDelivery
		if rows.Scan(&d.ID, &d.WebhookID, &d.DeliveryID, &d.Topic, &d.URL, &d.StatusCode, &d.DurationMS, &d.Response, &d.Error, &d.Success, &d.Created) == nil {
			out = append(out, d)
		}
	}
	return out
}

// gooWebhookPing sends WC_Webhook::deliver_ping()'s {"webhook_id": n} body once, synchronously, and
// returns the logged attempt (it does not count towards the failure limit).
func (s *Server) gooWebhookPing(ctx context.Context, id int64) (*gooWebhookDelivery, error) {
	w, err := s.gooWebhookByID(ctx, id)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"webhook_id": w.ID})
	h := gooWebhookRow{ID: w.ID, Topic: w.Topic, URL: w.URL, Secret: w.Secret, UserID: w.UserID, Failures: w.Failures}
	source := strings.TrimRight(s.links(ctx).Home, "/") + "/"
	_, _, rec := s.gooWebhookAttempt(ctx, h, source, body, gooRandomKey(32))
	return &rec, nil
}

// gooWebhookResult records a delivery's outcome: success resets the failure count, a failure adds one
// and disables the webhook at gooWebhookMaxFailures.
func (s *Server) gooWebhookResult(ctx context.Context, h gooWebhookRow, ok bool) {
	if ok {
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_webhooks SET failures = 0 WHERE id = $1`, h.ID)
		return
	}
	var failures int
	if err := s.store.Pool.QueryRow(ctx, `UPDATE goo_webhooks SET failures = failures + 1,
		status = CASE WHEN failures + 1 >= $2 THEN 'disabled' ELSE status END WHERE id = $1 RETURNING failures`, h.ID, gooWebhookMaxFailures).Scan(&failures); err == nil && failures >= gooWebhookMaxFailures {
		log.Printf("goocommerce: webhook %d (%s) disabled after %d failed deliveries", h.ID, h.Topic, failures)
	}
}
