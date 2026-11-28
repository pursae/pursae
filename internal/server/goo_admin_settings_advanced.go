package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Settings › Advanced › REST API (section=keys) and Webhooks (section=webhooks).

type gooKeyRow struct {
	ID          int64
	Description string
	Truncated   string
	User        string
	Permissions string
	LastAccess  string
	EditURL     string
}

type gooKeyEdit struct {
	ID          int64
	Description string
	UserID      string
	Users       []gooOpt
	Permissions string
	Perms       []gooOpt
	Truncated   string
	LastAccess  string
	// Shown once, right after the key was generated.
	ConsumerKey    string
	ConsumerSecret string
}

var gooKeyPerms = []gooOpt{{"read", "Read"}, {"write", "Write"}, {"read_write", "Read/Write"}}

func gooPermLabel(v string) string {
	for _, p := range gooKeyPerms {
		if p.Value == v {
			return p.Label
		}
	}
	return v
}

func (s *Server) gooKeysView(c *gin.Context, d *gooSettingsData) {
	ctx := c.Request.Context()
	d.Title = "REST API"
	if c.Query("create-key") != "" || c.Query("edit-key") != "" {
		d.Kind = "key"
		k := &gooKeyEdit{Perms: gooKeyPerms, Permissions: "read", Users: s.gooCustomerOptions(ctx)}
		if u := currentUser(c); u != nil {
			k.UserID = itoa64(u.ID)
		}
		if id, _ := strconv.ParseInt(c.Query("edit-key"), 10, 64); id > 0 {
			var uid int64
			var last *time.Time
			if err := s.store.Pool.QueryRow(ctx, `SELECT id, description, user_id, permissions, truncated_key, last_access FROM goo_api_keys WHERE id = $1`, id).
				Scan(&k.ID, &k.Description, &uid, &k.Permissions, &k.Truncated, &last); err == nil {
				k.UserID = itoa64(uid)
				k.LastAccess = "Unknown"
				if last != nil {
					k.LastAccess = last.In(s.loc()).Format("January 2, 2006 @ 3:04 pm")
				}
			}
		}
		d.Key = k
		d.BackURL, d.BackText = gooAdminURL("settings", "tab", "advanced", "section", "keys"), "REST API"
		d.HideSave = true
		return
	}
	d.Kind, d.HideSave = "keys", true
	rows, err := s.store.Pool.Query(ctx, `SELECT k.id, k.description, k.truncated_key, coalesce(u.display_name, ''), k.permissions, k.last_access
		FROM goo_api_keys k LEFT JOIN users u ON u.id = k.user_id ORDER BY k.id DESC`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r gooKeyRow
		var last *time.Time
		if rows.Scan(&r.ID, &r.Description, &r.Truncated, &r.User, &r.Permissions, &last) == nil {
			r.Permissions = gooPermLabel(r.Permissions)
			r.LastAccess = "Unknown"
			if last != nil {
				r.LastAccess = last.In(s.loc()).Format("January 2, 2006 @ 3:04 pm")
			}
			r.EditURL = gooAdminURL("settings", "tab", "advanced", "section", "keys", "edit-key", itoa64(r.ID))
			d.Keys = append(d.Keys, r)
		}
	}
}

func gooRandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// gooKeysPost generates, updates or revokes a key. A generated key is shown once on the response page.
func (s *Server) gooKeysPost(c *gin.Context) {
	ctx := c.Request.Context()
	back := gooAdminURL("settings", "tab", "advanced", "section", "keys")
	do := c.PostForm("goo_do")
	me := currentUser(c)
	// Like WooCommerce, a key may only belong to (or be taken from) a user the current user may edit:
	// a shop manager (manage_woocommerce without edit_users) handles their own keys only, so they cannot
	// mint a key that signs in as an administrator.
	canAssign := func(uid int64) bool { return uid == me.ID || me.Can("edit_users", nil) }
	keyOwner := func(id int64) (int64, bool) {
		var owner int64
		err := s.store.Pool.QueryRow(ctx, `SELECT user_id FROM goo_api_keys WHERE id = $1`, id).Scan(&owner)
		return owner, err == nil
	}
	const denied = "You do not have permission to assign API Keys to the selected user."
	if id, ok := strings.CutPrefix(do, "revoke:"); ok {
		n, _ := strconv.ParseInt(id, 10, 64)
		if owner, found := keyOwner(n); found && !canAssign(owner) {
			gooRedirect(c, back, "", "You do not have permission to revoke this API Key.")
			return
		}
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_api_keys WHERE id = $1`, n)
		gooRedirect(c, back, "API key revoked.", "")
		return
	}
	desc := strings.TrimSpace(c.PostForm("key_description"))
	uid, _ := strconv.ParseInt(c.PostForm("key_user"), 10, 64)
	perm := c.PostForm("key_permissions")
	if perm != "read" && perm != "write" && perm != "read_write" {
		perm = "read"
	}
	if desc == "" {
		gooRedirect(c, back+"&create-key=1", "", "Description is missing.")
		return
	}
	if _, err := s.store.UserByID(ctx, uid); err != nil {
		gooRedirect(c, back+"&create-key=1", "", "User is missing.")
		return
	}
	if !canAssign(uid) {
		gooRedirect(c, back+"&create-key=1", "", denied)
		return
	}
	if id, _ := strconv.ParseInt(c.PostForm("key_id"), 10, 64); id > 0 {
		if owner, found := keyOwner(id); found && !canAssign(owner) {
			gooRedirect(c, back+"&edit-key="+itoa64(id), "", denied)
			return
		}
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_api_keys SET description = $2, user_id = $3, permissions = $4 WHERE id = $1`, id, desc, uid, perm)
		gooRedirect(c, back+"&edit-key="+itoa64(id), "API Key updated successfully.", "")
		return
	}
	ck, cs := "ck_"+gooRandHex(20), "cs_"+gooRandHex(20)
	sum := sha256.Sum256([]byte(ck))
	var id int64
	if err := s.store.Pool.QueryRow(ctx, `INSERT INTO goo_api_keys (user_id, description, permissions, consumer_key, consumer_secret, truncated_key)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, uid, desc, perm, hex.EncodeToString(sum[:]), gooHashSecret(cs), ck[len(ck)-7:]).Scan(&id); err != nil {
		gooRedirect(c, back, "", err.Error())
		return
	}
	d := s.gooSettingsShell(c, gooSettingsTabs(), gooTabByID("advanced"), "keys")
	d.Kind, d.Title, d.HideSave = "key", "REST API", true
	d.BackURL, d.BackText = back, "REST API"
	d.Key = &gooKeyEdit{ID: id, Description: desc, UserID: itoa64(uid), Users: s.gooCustomerOptions(ctx), Permissions: perm, Perms: gooKeyPerms,
		Truncated: ck[len(ck)-7:], LastAccess: "Unknown", ConsumerKey: ck, ConsumerSecret: cs}
	s.gooStoreRender(c, "goocommerce?page=settings", "wc-settings", "Advanced", "admin/goo-settings", d,
		notice{Type: "success", Message: template.HTML("API Key generated successfully. Make sure to copy your new keys now as the secret key will be hidden once you leave this page.")})
}

func gooTabByID(id string) *gooTab {
	tabs := gooSettingsTabs()
	for i := range tabs {
		if tabs[i].ID == id {
			return &tabs[i]
		}
	}
	return &tabs[0]
}

// Webhooks ---------------------------------------------------------------------------------------------------

type gooHookRow struct {
	ID                       int64
	Name, Status, Topic, URL string
	EditURL                  string
	Failures                 int
}

type gooHookEdit struct {
	ID          int64
	Name        string
	Status      string
	Topic       string
	ActionEvent string
	Deliveries  []gooHookDelivery
	URL         string
	Secret      string
	Statuses    []gooOpt
	Topics      []gooOpt
	Created     string
}

// gooHookDelivery is a row of a webhook's delivery log.
type gooHookDelivery struct {
	Time, Topic, Result, Duration, Body string
	OK                                  bool
}

var gooWebhookStatuses = []gooOpt{{"active", "Active"}, {"paused", "Paused"}, {"disabled", "Disabled"}}

var gooWebhookTopics = []gooOpt{
	{"coupon.created", "Coupon created"}, {"coupon.updated", "Coupon updated"}, {"coupon.deleted", "Coupon deleted"}, {"coupon.restored", "Coupon restored"},
	{"customer.created", "Customer created"}, {"customer.updated", "Customer updated"}, {"customer.deleted", "Customer deleted"},
	{"order.created", "Order created"}, {"order.updated", "Order updated"}, {"order.deleted", "Order deleted"}, {"order.restored", "Order restored"},
	{"product.created", "Product created"}, {"product.updated", "Product updated"}, {"product.deleted", "Product deleted"}, {"product.restored", "Product restored"},
	{"action", "Action"},
}

// gooWebhookModuleTopics are the topics of the optional modules, offered while the module is on.
var gooWebhookModuleTopics = []struct {
	Module string
	Topics []gooOpt
}{
	{gsubKey, []gooOpt{{"subscription.created", "Subscription created"}, {"subscription.updated", "Subscription updated"}, {"subscription.deleted", "Subscription deleted"}}},
	{bkKey, []gooOpt{{"booking.created", "Booking created"}, {"booking.updated", "Booking updated"}}},
	{gmemKey, []gooOpt{{"membership.created", "Membership created"}, {"membership.updated", "Membership updated"}}},
}

// gooWebhookTopicOpts are the topics a webhook can listen to now: the core ones plus those of the
// modules that are switched on (kept before the generic "Action" entry).
func (s *Server) gooWebhookTopicOpts() []gooOpt {
	out := make([]gooOpt, 0, len(gooWebhookTopics)+6)
	out = append(out, gooWebhookTopics[:len(gooWebhookTopics)-1]...)
	for _, m := range gooWebhookModuleTopics {
		if s.gooExtOn(m.Module) {
			out = append(out, m.Topics...)
		}
	}
	return append(out, gooWebhookTopics[len(gooWebhookTopics)-1])
}

// gooWebhookTopicValid reports whether topic can be stored: a listed topic or action.<hook>.
func (s *Server) gooWebhookTopicValid(topic string) bool {
	if strings.HasPrefix(topic, "action.") {
		return len(topic) > len("action.")
	}
	if topic == "action" {
		return false // an action webhook needs the hook name
	}
	return gooOptLabel(s.gooWebhookTopicOpts(), topic) != topic
}

func gooOptLabel(list []gooOpt, v string) string {
	for _, o := range list {
		if o.Value == v {
			return o.Label
		}
	}
	return v
}

func (s *Server) gooWebhooksView(c *gin.Context, d *gooSettingsData) {
	ctx := c.Request.Context()
	d.Title = "Webhooks"
	if raw := c.Query("edit-webhook"); raw != "" {
		d.Kind = "webhook"
		h := &gooHookEdit{Status: "active", Topic: "order.created", Statuses: gooWebhookStatuses, Topics: s.gooWebhookTopicOpts(), Secret: gooRandHex(16)}
		if id, _ := strconv.ParseInt(raw, 10, 64); id > 0 {
			var created time.Time
			if err := s.store.Pool.QueryRow(ctx, `SELECT id, name, status, topic, delivery_url, secret, created_at FROM goo_webhooks WHERE id = $1`, id).
				Scan(&h.ID, &h.Name, &h.Status, &h.Topic, &h.URL, &h.Secret, &created); err == nil {
				h.Secret = s.revealSecret(h.Secret) // stored sealed (secret_seal.go)
				h.Created = created.In(s.loc()).Format("January 2, 2006 @ 3:04 pm")
				if ev, ok := strings.CutPrefix(h.Topic, "action."); ok {
					h.Topic, h.ActionEvent = "action", ev
				}
				for _, dl := range s.gooWebhookDeliveries(ctx, id, 20) {
					res := fmt.Sprintf("HTTP %d", dl.StatusCode)
					if dl.Error != "" {
						res = dl.Error
					}
					h.Deliveries = append(h.Deliveries, gooHookDelivery{Time: dl.Created.In(s.loc()).Format("Jan 2, 2006 3:04:05 pm"), Topic: dl.Topic, Result: res,
						Duration: fmt.Sprintf("%d ms", dl.DurationMS), Body: dl.Response, OK: dl.Success})
				}
			}
		}
		if h.ID == 0 {
			h.Name = "Webhook created on " + time.Now().In(s.loc()).Format("January 2, 2006 @ 3:04 PM")
		}
		d.Hook = h
		d.BackURL, d.BackText = gooAdminURL("settings", "tab", "advanced", "section", "webhooks"), "Webhooks"
		d.SaveText = "Save webhook"
		return
	}
	d.Kind, d.HideSave = "webhooks", true
	rows, err := s.store.Pool.Query(ctx, `SELECT id, name, status, topic, delivery_url, failures FROM goo_webhooks ORDER BY id DESC`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r gooHookRow
		if rows.Scan(&r.ID, &r.Name, &r.Status, &r.Topic, &r.URL, &r.Failures) == nil {
			r.Status = gooOptLabel(gooWebhookStatuses, r.Status)
			r.EditURL = gooAdminURL("settings", "tab", "advanced", "section", "webhooks", "edit-webhook", itoa64(r.ID))
			d.Hooks = append(d.Hooks, r)
		}
	}
}

func (s *Server) gooWebhooksPost(c *gin.Context) {
	ctx := c.Request.Context()
	back := gooAdminURL("settings", "tab", "advanced", "section", "webhooks")
	id, _ := strconv.ParseInt(c.PostForm("webhook_id"), 10, 64)
	if raw, ok := strings.CutPrefix(c.PostForm("goo_do"), "delete:"); ok {
		n, _ := strconv.ParseInt(raw, 10, 64)
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_webhooks WHERE id = $1`, n)
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM goo_webhook_deliveries WHERE webhook_id = $1`, n)
		gooRedirect(c, back, "Webhook permanently deleted.", "")
		return
	}
	if c.PostForm("goo_do") == "ping" && id > 0 {
		rec, err := s.gooWebhookPing(ctx, id)
		if err != nil {
			gooRedirect(c, back, "", "Webhook not found.")
			return
		}
		if rec.Success {
			gooRedirect(c, back+"&edit-webhook="+itoa64(id), fmt.Sprintf("Test delivery sent: HTTP %d.", rec.StatusCode), "")
		} else {
			msg := rec.Error
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", rec.StatusCode)
			}
			gooRedirect(c, back+"&edit-webhook="+itoa64(id), "", "Test delivery failed: "+msg)
		}
		return
	}
	name := strings.TrimSpace(c.PostForm("webhook_name"))
	status := c.PostForm("webhook_status")
	topic := c.PostForm("webhook_topic")
	if topic == "action" {
		topic += "." + strings.TrimSpace(c.PostForm("webhook_action_event"))
	}
	delivery := strings.TrimSpace(c.PostForm("webhook_delivery_url"))
	secret := strings.TrimSpace(c.PostForm("webhook_secret"))
	editURL := back + "&edit-webhook=" + itoa64(id)
	if u, err := url.Parse(delivery); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		gooRedirect(c, editURL, "", "Webhook delivery URL must be a valid URL starting with http:// or https://")
		return
	}
	if gooOptLabel(gooWebhookStatuses, status) == status {
		status = "active"
	}
	if !s.gooWebhookTopicValid(topic) {
		gooRedirect(c, editURL, "", "Webhook topic unknown. Please select a valid topic.")
		return
	}
	if name == "" {
		name = "Webhook created on " + time.Now().In(s.loc()).Format("January 2, 2006 @ 3:04 PM")
	}
	var uid int64
	if u := currentUser(c); u != nil {
		uid = u.ID
	}
	if id > 0 {
		_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_webhooks SET name=$2, status=$3, topic=$4, delivery_url=$5, secret=$6 WHERE id=$1`, id, name, status, topic, delivery, s.sealSecret(secret))
	} else if err := s.store.Pool.QueryRow(ctx, `INSERT INTO goo_webhooks (name, status, topic, delivery_url, secret, user_id) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		name, status, topic, delivery, s.sealSecret(secret), uid).Scan(&id); err != nil {
		gooRedirect(c, back, "", err.Error())
		return
	}
	gooRedirect(c, back+"&edit-webhook="+itoa64(id), "Webhook updated successfully.", "")
}
