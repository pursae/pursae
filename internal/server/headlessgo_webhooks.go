package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"wpgo/internal/auth"
)

// HeadlessGo webhooks: on entry create, update, publish, unpublish and delete (and, optionally,
// schema changes) every enabled webhook subscribed to the event receives a JSON POST. Deliveries
// go through a durable queue (hg_webhook_queue): the first attempt starts at once in the
// background; a failure (a network error or a non-2xx answer) is retried with an exponential
// backoff until the webhook's maximum of attempts, in-process and by the scheduler's tick after a
// restart. Each attempt is logged with its status code, duration and the start of the response. With a secret, the body is
// signed: X-HeadlessGo-Signature: sha256=<hex HMAC-SHA256 of the body>.

type hgWebhook struct {
	ID        int64
	Name      string
	URL       string
	Events    []string
	Headers   map[string]string
	Secret    string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

var hgWebhookEvents = []struct{ Key, Label string }{
	{"entry.create", "Entry created"}, {"entry.update", "Entry updated"}, {"entry.delete", "Entry deleted"},
	{"entry.publish", "Entry published"}, {"entry.unpublish", "Entry unpublished"}, {"schema.update", "Content type changed"},
}

// hgWebhookBase is the wait before the first retry; each further retry waits four times longer
// (10 s, 40 s, 160 s, 640 s…, at most six hours). Tests shorten it.
var hgWebhookBase = 10 * time.Second

// hgWebhookBackoff is the wait before attempt n+1 after n failed attempts.
func hgWebhookBackoff(n int) time.Duration {
	d := hgWebhookBase
	for i := 1; i < n; i++ {
		d *= 4
		if d > 6*time.Hour {
			return 6 * time.Hour
		}
	}
	return d
}

// hgWebhookClient delivers webhooks to public addresses only, like wp_safe_remote_request and the
// core's feed and plugin fetchers: the address is checked when the connection is made (after DNS
// resolution, so a host name pointing at 127.0.0.1 is refused too), and redirects are not
// followed. A delivery URL cannot reach the server's own services, the private network or a
// cloud metadata endpoint — the log shows the start of the answer, so that would be readable.
var hgWebhookClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{TLSHandshakeTimeout: 10 * time.Second, DialContext: (&net.Dialer{Timeout: 10 * time.Second,
		Control: publicOnly(hgWebhookAllowPrivate.Load, "the webhook URL points at a private network address")}).DialContext},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// hgWebhookAllowPrivate lets tests deliver to httptest servers on the loopback address.
var hgWebhookAllowPrivate atomic.Bool

// hgWebhookWG tracks in-process deliveries and pending in-process retries (tests wait on it).
var hgWebhookWG sync.WaitGroup

// hgReservedHeaders cannot be set as custom headers.
var hgReservedHeaders = map[string]bool{"content-type": true, "content-length": true, "host": true, "user-agent": true,
	"x-headlessgo-event": true, "x-headlessgo-delivery": true, "x-headlessgo-signature": true}

const hgWebhookCols = `id, name, url, events, headers, secret, enabled, created_at, updated_at`

func hgScanWebhook(row interface{ Scan(...any) error }) (*hgWebhook, error) {
	w := &hgWebhook{}
	var hdr []byte
	if err := row.Scan(&w.ID, &w.Name, &w.URL, &w.Events, &hdr, &w.Secret, &w.Enabled, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(hdr, &w.Headers)
	if w.Headers == nil {
		w.Headers = map[string]string{}
	}
	return w, nil
}

func (s *Server) hgWebhooks(ctx context.Context) []*hgWebhook {
	rows, err := s.store.Pool.Query(ctx, `SELECT `+hgWebhookCols+` FROM hg_webhooks ORDER BY lower(name), id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*hgWebhook
	for rows.Next() {
		if w, err := hgScanWebhook(rows); err == nil {
			out = append(out, s.hgRevealWebhook(w))
		}
	}
	return out
}

// The signing secret and the custom header values (an Authorization token, an API key) are stored
// sealed (sealSecret); a webhook read from the table holds them in clear. A plain value (a row from
// before sealing) reads as it is, and sealWebhookSecrets seals it at start-up.
func (s *Server) hgRevealWebhook(w *hgWebhook) *hgWebhook {
	w.Secret = s.revealSecret(w.Secret)
	for k, v := range w.Headers {
		w.Headers[k] = s.revealSecret(v)
	}
	return w
}

func (s *Server) hgSealHeaders(hdr map[string]string) []byte {
	sealed := make(map[string]string, len(hdr))
	for k, v := range hdr {
		sealed[k] = s.sealSecret(v)
	}
	raw, _ := json.Marshal(sealed)
	return raw
}

func (s *Server) hgWebhookByID(ctx context.Context, id int64) *hgWebhook {
	w, err := hgScanWebhook(s.store.Pool.QueryRow(ctx, `SELECT `+hgWebhookCols+` FROM hg_webhooks WHERE id = $1`, id))
	if err != nil {
		return nil
	}
	return s.hgRevealWebhook(w)
}

// hgSaveWebhook validates and stores a webhook (ID 0 creates it).
func (s *Server) hgSaveWebhook(ctx context.Context, w *hgWebhook) error {
	w.Name, w.URL = strings.TrimSpace(w.Name), strings.TrimSpace(w.URL)
	if w.Name == "" {
		return errors.New("The webhook needs a name.")
	}
	if !hgHTTPURL(w.URL) {
		return errors.New("The URL must be a full http:// or https:// address.")
	}
	var events []string
	for _, e := range w.Events {
		for _, k := range hgWebhookEvents {
			if k.Key == e && !containsStr(events, e) {
				events = append(events, e)
			}
		}
	}
	if len(events) == 0 {
		return errors.New("Choose at least one event.")
	}
	w.Events = events
	hdr := map[string]string{}
	for k, v := range w.Headers {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if hgReservedHeaders[strings.ToLower(k)] || strings.ContainsAny(k, " :\r\n") || strings.ContainsAny(v, "\r\n") {
			return errors.New("The header " + k + " cannot be set.")
		}
		hdr[k] = strings.TrimSpace(v)
	}
	w.Headers = hdr
	raw, secret := s.hgSealHeaders(hdr), s.sealSecret(w.Secret)
	if w.ID == 0 {
		return s.store.Pool.QueryRow(ctx, `INSERT INTO hg_webhooks(name, url, events, headers, secret, enabled) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			w.Name, w.URL, w.Events, raw, secret, w.Enabled).Scan(&w.ID)
	}
	_, err := s.store.Pool.Exec(ctx, `UPDATE hg_webhooks SET name = $2, url = $3, events = $4, headers = $5, secret = $6, enabled = $7, updated_at = now() WHERE id = $1`,
		w.ID, w.Name, w.URL, w.Events, raw, secret, w.Enabled)
	return err
}

func (s *Server) hgDeleteWebhook(ctx context.Context, id int64) error {
	_, err := s.store.Pool.Exec(ctx, `DELETE FROM hg_webhooks WHERE id = $1`, id)
	return err
}

// hgEntryPayload is an entry as webhooks receive it: every public scalar attribute of its draft
// (or, for publish events, published) version, without populated relations.
func (s *Server) hgEntryPayload(ctx context.Context, sc *hgSchema, e *hgEntry, status string) map[string]any {
	h := &hgSer{s: s, reg: s.hgReg(ctx), a: &hgAuth{Kind: "token", Token: &hgToken{Kind: "full-access"}}, p: &hgReadParams{Status: status, Locale: e.Locale}}
	data := e.Version(status)
	if data == nil {
		data = e.Data
	}
	out, err := h.object(sc, data, nil, nil, 0)
	if err != nil {
		out = map[string]any{}
	}
	out["id"], out["documentId"] = e.ID, e.DocumentID
	out["createdAt"], out["updatedAt"] = hgTime(e.CreatedAt), hgTime(e.UpdatedAt)
	if e.PublishedAt != nil && status != "draft" {
		out["publishedAt"] = hgTime(*e.PublishedAt)
	} else {
		out["publishedAt"] = nil
	}
	if sc.Localized {
		out["locale"] = e.Locale
	}
	return out
}

// hgEmit sends an entry event to the subscribed webhooks.
func (s *Server) hgEmit(ctx context.Context, event string, sc *hgSchema, e *hgEntry) {
	s.hgPurge()                         // cached public reads (hgWriteJSON tags them)
	if b, ok := hgBrokers.Load(s); ok { // GraphQL subscriptions
		b.(*hgBroker).publish(hgSubEvent{Event: event, UID: sc.UID, Doc: e.DocumentID, Locale: e.Locale, WasPublished: e.Published != nil})
	}
	status := "draft"
	if event == "entry.publish" || !sc.DraftAndPublish || event == "entry.delete" && e.Published != nil {
		status = "published"
	}
	s.hgFireHook(ctx, event, sc, e, status)
	s.hgEmitPayload(ctx, event, map[string]any{"model": sc.SingularName, "uid": sc.UID, "entry": s.hgEntryPayload(ctx, sc, e, status)})
}

// hgEmitSchema sends schema.update when the setting asks for it.
func (s *Server) hgEmitSchema(ctx context.Context, action string, sc *hgSchema) {
	s.hgPurge()
	if !s.hgLoad().SchemaWebhooks {
		return
	}
	s.hgEmitPayload(ctx, "schema.update", map[string]any{"action": action, "uid": sc.UID, "schema": sc})
}

// hgEmitPayload queues the event for every enabled webhook subscribed to it and starts the first
// attempt in the background. The queue row is the delivery's durable state: retries survive a
// restart (hgProcessQueue).
func (s *Server) hgEmitPayload(ctx context.Context, event string, extra map[string]any) {
	hooks := s.hgWebhooks(ctx)
	max := s.hgLoad().WebhookAttempts
	queued := false
	for _, w := range hooks {
		if !w.Enabled || !containsStr(w.Events, event) {
			continue
		}
		payload := map[string]any{"event": event, "createdAt": hgTime(time.Now())}
		for k, v := range extra {
			payload[k] = v
		}
		body, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		var id int64
		if err := s.store.Pool.QueryRow(context.WithoutCancel(ctx), `INSERT INTO hg_webhook_queue(webhook_id, delivery_id, event, body, max_attempts)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, w.ID, auth.RandomToken(12), event, body, max).Scan(&id); err != nil {
			log.Printf("headlessgo: webhook queue: %v", err)
			continue
		}
		queued = true
		s.hgKickDelivery(id, 0)
	}
	if queued {
		s.hgEnsureTick(context.WithoutCancel(ctx))
	}
}

// hgInProcessRetry is the longest backoff retried by an in-process timer; longer ones (and every
// retry after a restart) are made by the scheduler's tick.
const hgInProcessRetry = 2 * time.Minute

// hgKickDelivery runs one queued delivery in the background after wait.
func (s *Server) hgKickDelivery(id int64, wait time.Duration) {
	hgWebhookWG.Add(1)
	run := func() {
		defer hgWebhookWG.Done()
		s.hgDeliverQueued(context.Background(), id)
	}
	if wait <= 0 {
		safeGo("headlessgo_webhooks.hgKickDelivery", run)
		return
	}
	time.AfterFunc(wait, run)
}

// hgDelivery is one logged attempt.
type hgDelivery struct {
	ID         int64
	WebhookID  int64
	DeliveryID string
	Event      string
	Attempt    int
	StatusCode int
	DurationMS int
	Success    bool
	Error      string
	Response   string
	CreatedAt  time.Time
}

// hgQueued is a claimed row of the delivery queue.
type hgQueued struct {
	ID, WebhookID      int64
	DeliveryID, Event  string
	Body               []byte
	Attempts, MaxTries int
}

const hgQueueCols = `id, webhook_id, delivery_id, event, body, attempts, max_attempts`

func hgScanQueued(row interface{ Scan(...any) error }) (*hgQueued, error) {
	q := &hgQueued{}
	return q, row.Scan(&q.ID, &q.WebhookID, &q.DeliveryID, &q.Event, &q.Body, &q.Attempts, &q.MaxTries)
}

// hgDeliverQueued claims one queue row if it is due and makes an attempt.
func (s *Server) hgDeliverQueued(ctx context.Context, id int64) {
	q, err := hgScanQueued(s.store.Pool.QueryRow(ctx, `UPDATE hg_webhook_queue SET status = 'running', locked_at = now()
		WHERE id = $1 AND status = 'pending' AND next_at <= now() RETURNING `+hgQueueCols, id))
	if err != nil {
		return // not due, or taken by the worker
	}
	s.hgAttemptQueued(ctx, q, true)
}

// hgProcessQueue makes an attempt for every due queue row (the scheduler's tick): retries whose
// in-process timer was lost to a restart, and rows of a crashed instance.
func (s *Server) hgProcessQueue(ctx context.Context) int {
	_, _ = s.store.Pool.Exec(ctx, `UPDATE hg_webhook_queue SET status = 'pending', locked_at = NULL WHERE status = 'running' AND locked_at < now() - interval '5 minutes'`)
	rows, err := s.store.Pool.Query(ctx, `UPDATE hg_webhook_queue SET status = 'running', locked_at = now()
		WHERE id IN (SELECT id FROM hg_webhook_queue WHERE status = 'pending' AND next_at <= now() ORDER BY next_at, id LIMIT 50 FOR UPDATE SKIP LOCKED)
		RETURNING `+hgQueueCols)
	if err != nil {
		return 0
	}
	var due []*hgQueued
	for rows.Next() {
		if q, err := hgScanQueued(rows); err == nil {
			due = append(due, q)
		}
	}
	rows.Close()
	s.hgAttemptBatch(ctx, due, hgTickWorkers, hgTickBudget)
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM hg_webhook_queue WHERE status IN ('done', 'failed') AND updated_at < now() - interval '1 day'`)
	return len(due)
}

// The tick runs inside the site's job runner (scheduled posts and every other job wait for it):
// it delivers its batch with a few workers and stops starting new attempts after hgTickBudget.
// Rows it did not get to go back to the queue for the next tick.
const (
	hgTickWorkers = 8
	hgTickBudget  = 30 * time.Second
)

// hgAttemptBatch makes an attempt for each claimed row, workers at a time, starting none after
// budget; the rows left over are released (pending again).
func (s *Server) hgAttemptBatch(ctx context.Context, due []*hgQueued, workers int, budget time.Duration) {
	deadline := time.Now().Add(budget)
	next := make(chan *hgQueued)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		safeGo("headlessgo_webhooks.hgAttemptBatch", func() {
			defer wg.Done()
			for q := range next {
				s.hgAttemptQueued(ctx, q, false)
			}
		})
	}
	var left []int64
	for _, q := range due {
		if time.Now().After(deadline) || ctx.Err() != nil {
			left = append(left, q.ID)
			continue
		}
		next <- q
	}
	close(next)
	wg.Wait()
	if len(left) > 0 {
		_, _ = s.store.Pool.Exec(context.WithoutCancel(ctx), `UPDATE hg_webhook_queue SET status = 'pending', locked_at = NULL WHERE id = ANY($1) AND status = 'running'`, left)
	}
}

// hgAttemptQueued posts a claimed delivery and records the outcome: done, another try after the
// backoff, or failed once max_attempts is reached.
func (s *Server) hgAttemptQueued(ctx context.Context, q *hgQueued, inProcess bool) {
	w := s.hgWebhookByID(ctx, q.WebhookID)
	if w == nil || !w.Enabled {
		_, _ = s.store.Pool.Exec(ctx, `UPDATE hg_webhook_queue SET status = 'failed', last_error = 'webhook disabled or deleted', locked_at = NULL, updated_at = now() WHERE id = $1`, q.ID)
		return
	}
	n := q.Attempts + 1
	res := s.hgAttempt(ctx, w, q.Event, q.DeliveryID, q.Body, n)
	switch {
	case res.Success:
		_, _ = s.store.Pool.Exec(ctx, `UPDATE hg_webhook_queue SET status = 'done', attempts = $2, last_error = '', locked_at = NULL, updated_at = now() WHERE id = $1`, q.ID, n)
	case n >= q.MaxTries:
		_, _ = s.store.Pool.Exec(ctx, `UPDATE hg_webhook_queue SET status = 'failed', attempts = $2, last_error = $3, locked_at = NULL, updated_at = now() WHERE id = $1`, q.ID, n, hgAttemptError(res))
	default:
		wait := hgWebhookBackoff(n)
		_, _ = s.store.Pool.Exec(ctx, `UPDATE hg_webhook_queue SET status = 'pending', attempts = $2, last_error = $3, next_at = now() + $4::interval, locked_at = NULL, updated_at = now() WHERE id = $1`,
			q.ID, n, hgAttemptError(res), fmt.Sprintf("%d milliseconds", wait.Milliseconds()))
		if inProcess && wait <= hgInProcessRetry {
			s.hgKickDelivery(q.ID, wait+5*time.Millisecond)
		}
	}
}

func hgAttemptError(d hgDelivery) string {
	if d.Error != "" {
		return d.Error
	}
	return fmt.Sprintf("HTTP %d", d.StatusCode)
}

// hgQueueItem is a row of the queue for the admin screen.
type hgQueueItem struct {
	ID        int64
	WebhookID int64
	Event     string
	Status    string
	Attempts  int
	MaxTries  int
	NextAt    time.Time
	LastError string
}

func (s *Server) hgQueueItems(ctx context.Context, webhookID int64, limit int) []hgQueueItem {
	q := `SELECT id, webhook_id, event, status, attempts, max_attempts, next_at, last_error FROM hg_webhook_queue WHERE status IN ('pending', 'running', 'failed')`
	args := []any{}
	if webhookID > 0 {
		q += ` AND webhook_id = $1`
		args = append(args, webhookID)
	}
	q += ` ORDER BY id DESC LIMIT ` + itoa64(int64(limit))
	rows, err := s.store.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []hgQueueItem
	for rows.Next() {
		var it hgQueueItem
		if rows.Scan(&it.ID, &it.WebhookID, &it.Event, &it.Status, &it.Attempts, &it.MaxTries, &it.NextAt, &it.LastError) == nil {
			out = append(out, it)
		}
	}
	return out
}

// hgRetryDelivery puts a failed (or waiting) delivery back for an immediate attempt.
func (s *Server) hgRetryDelivery(ctx context.Context, id int64) error {
	tag, err := s.store.Pool.Exec(ctx, `UPDATE hg_webhook_queue SET status = 'pending', next_at = now(), max_attempts = greatest(max_attempts, attempts + 1), updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'failed')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errHGNotFound
	}
	s.hgKickDelivery(id, 0)
	return nil
}

// hgAttempt posts the body once and logs the result.
func (s *Server) hgAttempt(ctx context.Context, w *hgWebhook, event, id string, body []byte, attempt int) hgDelivery {
	res := hgDelivery{WebhookID: w.ID, DeliveryID: id, Event: event, Attempt: attempt}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		res.Error = err.Error()
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "HeadlessGo-Webhook/"+hgVersion)
		for k, v := range w.Headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("X-HeadlessGo-Event", event)
		req.Header.Set("X-HeadlessGo-Delivery", id)
		if w.Secret != "" {
			req.Header.Set("X-HeadlessGo-Signature", "sha256="+hgSign(w.Secret, body))
		}
		start := time.Now()
		resp, err := hgWebhookClient.Do(req)
		res.DurationMS = int(time.Since(start).Milliseconds())
		if err != nil {
			res.Error = err.Error()
		} else {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			res.StatusCode = resp.StatusCode
			res.Response = strings.ToValidUTF8(string(snippet), "")
			if !utf8.ValidString(res.Response) {
				res.Response = ""
			}
			res.Success = resp.StatusCode >= 200 && resp.StatusCode < 300
		}
	}
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO hg_webhook_deliveries(webhook_id, delivery_id, event, attempt, status_code, duration_ms, success, error, response)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, w.ID, id, event, attempt, res.StatusCode, res.DurationMS, res.Success, res.Error, res.Response); err != nil {
		log.Printf("headlessgo: webhook log: %v", err)
	}
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM hg_webhook_deliveries WHERE webhook_id = $1 AND id NOT IN (
		SELECT id FROM hg_webhook_deliveries WHERE webhook_id = $1 ORDER BY id DESC LIMIT 200)`, w.ID)
	return res
}

func hgSign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// hgTestWebhook sends a test event once, synchronously.
func (s *Server) hgTestWebhook(ctx context.Context, w *hgWebhook) hgDelivery {
	body, _ := json.Marshal(map[string]any{"event": "trigger-test", "createdAt": hgTime(time.Now())})
	return s.hgAttempt(ctx, w, "trigger-test", auth.RandomToken(12), body, 1)
}

func (s *Server) hgDeliveries(ctx context.Context, webhookID int64, limit int) []hgDelivery {
	q := `SELECT id, webhook_id, delivery_id, event, attempt, status_code, duration_ms, success, error, response, created_at FROM hg_webhook_deliveries`
	args := []any{}
	if webhookID > 0 {
		q += ` WHERE webhook_id = $1`
		args = append(args, webhookID)
	}
	q += ` ORDER BY id DESC LIMIT ` + itoa64(int64(limit))
	rows, err := s.store.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []hgDelivery
	for rows.Next() {
		var d hgDelivery
		if rows.Scan(&d.ID, &d.WebhookID, &d.DeliveryID, &d.Event, &d.Attempt, &d.StatusCode, &d.DurationMS, &d.Success, &d.Error, &d.Response, &d.CreatedAt) == nil {
			out = append(out, d)
		}
	}
	return out
}
