package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// wc/v3/webhooks (WC_REST_Webhooks_Controller): list, create, read, update, delete and batch the rows
// of goo_webhooks that Settings › Advanced › Webhooks edits. Webhooks cannot be trashed: deleting
// needs force=true. The secret is write-only.

func (s *Server) registerGooV3Webhooks() {
	mw := gooCapsAll("manage_woocommerce")
	s.gooV3("/webhooks", mw, map[string]restHandler{http.MethodGet: s.gooV3WebhooksList, http.MethodPost: s.gooV3Create(s.gooV3WebhookCreate)})
	s.gooV3("/webhooks/batch", mw, map[string]restHandler{http.MethodPost: func(c *gin.Context, req *restRequest) {
		s.gooBatch(c, req, gooBatchOps{Create: s.gooV3WebhookCreate, Update: s.gooV3WebhookUpdate, Delete: s.gooV3WebhookDelete})
	}})
	s.gooV3("/webhooks/(?P<id>[\\d]+)", mw, map[string]restHandler{
		http.MethodGet: s.gooV3Get(func(c *gin.Context, id int64) (map[string]any, *gooAPIErr) {
			w, err := s.gooWebhookByID(c.Request.Context(), id)
			if err != nil {
				return nil, gooErr(404, "woocommerce_rest_webhook_invalid_id", "Invalid ID.")
			}
			return s.gooV3WebhookJSON(c.Request.Context(), w), nil
		}),
		http.MethodPut:    s.gooV3Update(s.gooV3WebhookUpdate),
		http.MethodDelete: s.gooV3Delete(s.gooV3WebhookDelete),
	})
	// The delivery log and a test delivery (WC_REST_Webhooks_V1's deliveries, WC_Webhook::deliver_ping()).
	s.gooV3("/webhooks/(?P<id>[\\d]+)/deliveries", mw, map[string]restHandler{http.MethodGet: func(c *gin.Context, req *restRequest) {
		ctx := c.Request.Context()
		if _, err := s.gooWebhookByID(ctx, req.ID()); err != nil {
			restErr(c, http.StatusNotFound, "woocommerce_rest_webhook_invalid_id", "Invalid ID.")
			return
		}
		out := []any{}
		for _, d := range s.gooWebhookDeliveries(ctx, req.ID(), gooWebhookKeepDeliveries) {
			out = append(out, gooV3DeliveryJSON(&d))
		}
		restJSON(c, http.StatusOK, out)
	}})
	s.gooV3("/webhooks/(?P<id>[\\d]+)/ping", mw, map[string]restHandler{http.MethodPost: func(c *gin.Context, req *restRequest) {
		rec, err := s.gooWebhookPing(c.Request.Context(), req.ID())
		if err != nil {
			restErr(c, http.StatusNotFound, "woocommerce_rest_webhook_invalid_id", "Invalid ID.")
			return
		}
		restJSON(c, http.StatusOK, gooV3DeliveryJSON(rec))
	}})
}

// gooWebhook is a stored webhook as the API sees it.
type gooWebhook struct {
	ID                               int64
	Name, Status, Topic, URL, Secret string
	UserID                           int64
	Failures                         int
	Created                          time.Time
}

func (s *Server) gooWebhookByID(ctx context.Context, id int64) (*gooWebhook, error) {
	w := &gooWebhook{}
	err := s.store.Pool.QueryRow(ctx, `SELECT id, name, status, topic, delivery_url, secret, user_id, failures, created_at FROM goo_webhooks WHERE id = $1`, id).
		Scan(&w.ID, &w.Name, &w.Status, &w.Topic, &w.URL, &w.Secret, &w.UserID, &w.Failures, &w.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	w.Secret = s.revealSecret(w.Secret) // stored sealed (secret_seal.go)
	return w, err
}

func (s *Server) gooV3WebhookJSON(ctx context.Context, w *gooWebhook) map[string]any {
	resource, event, _ := strings.Cut(w.Topic, ".")
	hooks := []string{}
	if resource == "action" {
		hooks = append(hooks, event)
	} else if resource != "" {
		hooks = append(hooks, "woocommerce_"+event+"_"+resource)
	}
	return map[string]any{"id": w.ID, "name": w.Name, "status": w.Status, "topic": w.Topic, "resource": resource, "event": event,
		"hooks": hooks, "delivery_url": w.URL,
		"date_created": s.gooDate(&w.Created), "date_created_gmt": gooDateGMT(&w.Created),
		"date_modified": s.gooDate(&w.Created), "date_modified_gmt": gooDateGMT(&w.Created),
		"_links": s.gooLinks(ctx, "webhooks", w.ID)}
}

// gooV3ApplyWebhook copies the request's fields onto a webhook and validates them.
func (s *Server) gooV3ApplyWebhook(w *gooWebhook, b map[string]any) *gooAPIErr {
	if v, ok := b["name"]; ok {
		w.Name = strings.TrimSpace(gooStr(v))
	}
	if v, ok := b["status"]; ok {
		st := gooStr(v)
		if gooOptLabel(gooWebhookStatuses, st) == st {
			return gooErr(400, "rest_invalid_param", "Invalid parameter(s): status")
		}
		w.Status = st
	}
	if v, ok := b["topic"]; ok {
		t := strings.TrimSpace(gooStr(v))
		if !s.gooWebhookTopicValid(t) {
			return gooErr(400, "woocommerce_rest_webhook_invalid_topic", "Webhook topic is required and must be valid.")
		}
		w.Topic = t
	}
	if v, ok := b["delivery_url"]; ok {
		u, err := url.Parse(strings.TrimSpace(gooStr(v)))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return gooErr(400, "woocommerce_rest_webhook_invalid_delivery_url", "Webhook delivery URL must be a valid URL starting with http:// or https://.")
		}
		w.URL = u.String()
	}
	if v, ok := b["secret"]; ok {
		w.Secret = gooStr(v)
	}
	return nil
}

func (s *Server) gooV3WebhookCreate(c *gin.Context, b map[string]any) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	if gooStr(b["topic"]) == "" {
		return nil, gooErr(400, "woocommerce_rest_webhook_invalid_topic", "Webhook topic is required and must be valid.")
	}
	if gooStr(b["delivery_url"]) == "" {
		return nil, gooErr(400, "woocommerce_rest_webhook_invalid_delivery_url", "Webhook delivery URL must be a valid URL starting with http:// or https://.")
	}
	w := &gooWebhook{Status: "active", UserID: gooActorID(c)}
	if aerr := s.gooV3ApplyWebhook(w, b); aerr != nil {
		return nil, aerr
	}
	if w.Name == "" {
		w.Name = "Webhook created on " + time.Now().In(s.loc()).Format("Jan 02, 2006 @ 03:04 PM")
	}
	if w.Secret == "" {
		w.Secret = gooRandomKey(32)
	}
	if err := s.store.Pool.QueryRow(ctx, `INSERT INTO goo_webhooks (name, status, topic, delivery_url, secret, user_id) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		w.Name, w.Status, w.Topic, w.URL, s.sealSecret(w.Secret), w.UserID).Scan(&w.ID); err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_create", err.Error())
	}
	w, _ = s.gooWebhookByID(ctx, w.ID)
	return s.gooV3WebhookJSON(ctx, w), nil
}

func (s *Server) gooV3WebhookUpdate(c *gin.Context, id int64, b map[string]any) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	w, err := s.gooWebhookByID(ctx, id)
	if err != nil {
		return nil, gooErr(404, "woocommerce_rest_webhook_invalid_id", "Invalid ID.")
	}
	wasDisabled := w.Status != "active"
	if aerr := s.gooV3ApplyWebhook(w, b); aerr != nil {
		return nil, aerr
	}
	if wasDisabled && w.Status == "active" {
		w.Failures = 0 // re-activating starts the failure count over
	}
	if _, err := s.store.Pool.Exec(ctx, `UPDATE goo_webhooks SET name=$2, status=$3, topic=$4, delivery_url=$5, secret=$6, failures=$7 WHERE id=$1`,
		w.ID, w.Name, w.Status, w.Topic, w.URL, s.sealSecret(w.Secret), w.Failures); err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_edit", err.Error())
	}
	return s.gooV3WebhookJSON(ctx, w), nil
}

func (s *Server) gooV3WebhookDelete(c *gin.Context, id int64, force bool) (map[string]any, *gooAPIErr) {
	ctx := c.Request.Context()
	w, err := s.gooWebhookByID(ctx, id)
	if err != nil {
		return nil, gooErr(404, "woocommerce_rest_webhook_invalid_id", "Invalid ID.")
	}
	if !force {
		return nil, gooErr(http.StatusNotImplemented, "woocommerce_rest_trash_not_supported", "Webhooks do not support trashing.")
	}
	obj := s.gooV3WebhookJSON(ctx, w)
	if _, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_webhooks WHERE id = $1`, id); err != nil {
		return nil, gooErr(500, "woocommerce_rest_cannot_delete", err.Error())
	}
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_webhook_deliveries WHERE webhook_id = $1`, id)
	return obj, nil
}

func (s *Server) gooV3WebhooksList(c *gin.Context, req *restRequest) {
	ctx := c.Request.Context()
	status := c.Query("status")
	if status != "" && status != "all" && gooOptLabel(gooWebhookStatuses, status) == status {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): status")
		return
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT id FROM goo_webhooks ORDER BY created_at DESC, id DESC`)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "woocommerce_rest_cannot_view", err.Error())
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	search := strings.ToLower(c.Query("search"))
	inc, exc := gooQueryIDs(c, "include"), gooQueryIDs(c, "exclude")
	var keep []*gooWebhook
	for _, id := range ids {
		w, err := s.gooWebhookByID(ctx, id)
		if err != nil || status != "" && status != "all" && w.Status != status || search != "" && !strings.Contains(strings.ToLower(w.Name), search) ||
			len(inc) > 0 && !gooInt64In(w.ID, inc) || gooInt64In(w.ID, exc) {
			continue
		}
		keep = append(keep, w)
	}
	if strings.EqualFold(c.Query("order"), "asc") {
		for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
			keep[i], keep[j] = keep[j], keep[i]
		}
	}
	out := []any{}
	for _, w := range gooPaginateSlice(c, keep) {
		out = append(out, s.gooV3WebhookJSON(ctx, w))
	}
	restJSON(c, http.StatusOK, out)
}

func gooV3DeliveryJSON(d *gooWebhookDelivery) map[string]any {
	t := d.Created
	if t.IsZero() {
		t = time.Now()
	}
	return map[string]any{"id": d.ID, "webhook_id": d.WebhookID, "delivery_id": d.DeliveryID, "topic": d.Topic, "delivery_url": d.URL,
		"success": d.Success, "response_code": d.StatusCode, "response_body": d.Response, "error": d.Error,
		"duration_ms": d.DurationMS, "date_created_gmt": gooDateGMT(&t)}
}
