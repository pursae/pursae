package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"mime"
	"net/mail"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"wpgo/internal/auth"
	"wpgo/internal/i18n"
	wpmail "wpgo/internal/mail"
	"wpgo/internal/wp"
)

// Goocommerce's transactional e-mails (WC_Emails): WooCommerce's e-mails registered with their
// default subjects and headings, sent on the order status transitions, refunds, customer notes, new
// accounts and stock events. Each one follows its settings (email_<id>_settings: enabled, recipient,
// subject, heading, additional_content, email_type), uses the store's From name / address and the
// e-mail template options, takes attachments from gooEmailAttachHooks and is queued with sendMail.

// gooStockEmails are the inventory notices, sent to the stock e-mail recipient.
var gooStockEmails = map[string]bool{"low_stock": true, "no_stock": true, "backorder": true}

func init() {
	for _, d := range []*gooEmailDef{
		{ID: "new_order", Title: "New order", Description: "New order emails are sent to chosen recipient(s) when a new order is received.",
			Subject: "[{site_title}]: New order #{order_number}", Heading: "New Order: #{order_number}", Additional: "Congratulations on the sale."},
		{ID: "cancelled_order", Title: "Cancelled order", Description: "Cancelled order emails are sent to chosen recipient(s) when orders have been marked cancelled (if they were previously processing or on-hold).",
			Subject: "[{site_title}]: Order #{order_number} has been cancelled", Heading: "Order Cancelled: #{order_number}", Additional: "Thanks for reading."},
		{ID: "failed_order", Title: "Failed order", Description: "Failed order emails are sent to chosen recipient(s) when orders have been marked failed (if they were previously pending or on-hold).",
			Subject: "[{site_title}]: Order #{order_number} has failed", Heading: "Order Failed: #{order_number}", Additional: "Hopefully they’ll be back."},
		{ID: "customer_on_hold_order", Title: "Order on-hold", Customer: true, Description: "This is an order notification sent to customers containing order details after an order is placed on-hold from Pending, Cancelled or Failed order status.",
			Subject: "Your {site_title} order has been received!", Heading: "Thank you for your order", Additional: "We look forward to fulfilling your order soon."},
		{ID: "customer_processing_order", Title: "Processing order", Customer: true, Description: "This is an order notification sent to customers containing order details after payment.",
			Subject: "Your {site_title} order has been received!", Heading: "Thank you for your order", Additional: "Thanks for using {site_url}!"},
		{ID: "customer_completed_order", Title: "Completed order", Customer: true, Description: "Order complete emails are sent to customers when their orders are marked completed and usually indicate that their orders have been shipped.",
			Subject: "Your {site_title} order is now complete", Heading: "Thanks for shopping with us", Additional: "Thanks for shopping with us."},
		{ID: "customer_refunded_order", Title: "Refunded order", Customer: true, Description: "Order refunded emails are sent to customers when their orders are refunded.",
			Subject: "Your {site_title} order #{order_number} has been refunded", Heading: "Order Refunded: {order_number}", Additional: "We hope to see you again soon."},
		{ID: "customer_invoice", Title: "Order details", Customer: true, Manual: true, Description: "Order detail emails can be sent to customers containing their order information and payment links.",
			Subject: "Details for order #{order_number} on {site_title}", Heading: "Details for order #{order_number}", Additional: "Thanks for using {site_url}!"},
		{ID: "customer_note", Title: "Customer note", Customer: true, Description: "Customer note emails are sent when you add a note to an order.",
			Subject: "Note added to your {site_title} order from {order_date}", Heading: "A note has been added to your order", Additional: "Thanks for reading."},
		{ID: "customer_reset_password", Title: "Reset password", Customer: true, Description: "Customer \"reset password\" emails are sent when customers reset their passwords.",
			Subject: "Password Reset Request for {site_title}", Heading: "Password Reset Request", Additional: "Thanks for reading."},
		{ID: "customer_new_account", Title: "New account", Customer: true, Description: "Customer \"new account\" emails are sent to the customer when a customer signs up via checkout or account pages.",
			Subject: "Your {site_title} account has been created!", Heading: "Welcome to {site_title}", Additional: "We look forward to seeing you soon."},
		{ID: "low_stock", Title: "Low stock", Description: "Sent to the stock notification recipient(s) when a product's stock reaches the low stock threshold.",
			Subject: "[{site_title}] Product low in stock", Heading: "Product low in stock"},
		{ID: "no_stock", Title: "Out of stock", Description: "Sent to the stock notification recipient(s) when a product runs out of stock.",
			Subject: "[{site_title}] Product out of stock", Heading: "Product out of stock"},
		{ID: "backorder", Title: "Backorder", Description: "Sent to the stock notification recipient(s) when an order puts a product on backorder.",
			Subject: "[{site_title}] Product backorder", Heading: "Product backorder"},
	} {
		gooRegisterEmail(d)
	}
	gooEmailSender = func(s *Server, ctx context.Context, id string, o *gooOrder, args map[string]any) error {
		_, err := s.gooSendEmail(ctx, id, o, args)
		return err
	}

	gooOn("order_status_changed", 10, func(s *Server, ctx context.Context, arg any) {
		ev, ok := arg.(*gooStatusChange)
		if !ok || ev.Order == nil {
			return
		}
		o, from, to := ev.Order, ev.From, ev.To
		if containsStr([]string{"pending", "failed", "cancelled", "checkout-draft"}, from) && containsStr([]string{"processing", "completed", "on-hold"}, to) && !o.NewOrderEmailSent {
			if sent, _ := s.gooSendEmail(ctx, "new_order", o, nil); sent {
				o.NewOrderEmailSent = true
				_, _ = s.store.Pool.Exec(ctx, `UPDATE goo_orders SET new_order_email_sent = true WHERE id = $1`, o.ID)
			}
		}
		switch {
		case to == "processing" && containsStr([]string{"pending", "failed", "on-hold", "cancelled", "checkout-draft"}, from):
			_, _ = s.gooSendEmail(ctx, "customer_processing_order", o, nil)
		case to == "on-hold" && containsStr([]string{"pending", "failed", "cancelled", "checkout-draft"}, from):
			_, _ = s.gooSendEmail(ctx, "customer_on_hold_order", o, nil)
		case to == "completed":
			_, _ = s.gooSendEmail(ctx, "customer_completed_order", o, nil)
		case to == "cancelled" && (from == "processing" || from == "on-hold"):
			_, _ = s.gooSendEmail(ctx, "cancelled_order", o, nil)
		case to == "failed" && (from == "pending" || from == "on-hold"):
			_, _ = s.gooSendEmail(ctx, "failed_order", o, nil)
		}
	})
	gooOn("order_fully_refunded", 10, func(s *Server, ctx context.Context, arg any) {
		if ev, ok := arg.(*gooRefundEvent); ok {
			_, _ = s.gooSendEmail(ctx, "customer_refunded_order", ev.Order, map[string]any{"partial": false, "refund": ev.Refund})
		}
	})
	gooOn("order_partially_refunded", 10, func(s *Server, ctx context.Context, arg any) {
		if ev, ok := arg.(*gooRefundEvent); ok {
			_, _ = s.gooSendEmail(ctx, "customer_refunded_order", ev.Order, map[string]any{"partial": true, "refund": ev.Refund})
		}
	})
	gooOn("customer_note", 10, func(s *Server, ctx context.Context, arg any) {
		if ev, ok := arg.(*gooNoteEvent); ok {
			_, _ = s.gooSendEmail(ctx, "customer_note", ev.Order, map[string]any{"note": ev.Note})
		}
	})
	gooOn("customer_created", 10, func(s *Server, ctx context.Context, arg any) {
		args := map[string]any{}
		switch v := arg.(type) {
		case *gooNewCustomer:
			args["user_id"], args["login"], args["to"], args["password_generated"] = v.UserID, v.Login, v.Email, v.PasswordGenerated
		case int64:
			args["user_id"] = v
		default:
			return
		}
		_, _ = s.gooSendEmail(ctx, "customer_new_account", nil, args)
	})
	gooOn("low_stock", 10, func(s *Server, ctx context.Context, arg any) {
		if p, ok := arg.(*gooProduct); ok {
			_, _ = s.gooSendEmail(ctx, "low_stock", nil, map[string]any{"product": p})
		}
	})
	gooOn("no_stock", 10, func(s *Server, ctx context.Context, arg any) {
		if p, ok := arg.(*gooProduct); ok {
			_, _ = s.gooSendEmail(ctx, "no_stock", nil, map[string]any{"product": p})
		}
	})
	gooOn("backorder", 10, func(s *Server, ctx context.Context, arg any) {
		if ev, ok := arg.(*gooBackorderEvent); ok {
			_, _ = s.gooSendEmail(ctx, "backorder", ev.Order, map[string]any{"product": ev.Product, "quantity": ev.Quantity})
		}
	})
}

// gooEmailPlaceholders are the placeholders of an e-mail's subject, heading and additional content.
func (s *Server) gooEmailPlaceholders(ctx context.Context, o *gooOrder) map[string]string {
	home := s.links(ctx).Home
	host := home
	if u, err := url.Parse(home); err == nil && u.Host != "" {
		host = u.Host
	}
	ph := map[string]string{"{site_title}": s.settings.String("blogname"), "{site_address}": host, "{site_url}": host,
		"{store_email}": s.gooEmailFromAddress()}
	if o != nil {
		ph["{order_number}"] = o.Number()
		ph["{order_date}"] = s.gooEmailDate(ctx, o.DateCreated)
		ph["{order_billing_full_name}"] = o.BillingName()
	}
	return ph
}

func gooReplacePlaceholders(v string, ph map[string]string) string {
	for k, x := range ph {
		v = strings.ReplaceAll(v, k, x)
	}
	return v
}

// gooEmailDate is wc_format_datetime(): the site's date format.
func (s *Server) gooEmailDate(ctx context.Context, t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	f := s.settings.String("date_format")
	if f == "" {
		f = "F j, Y"
	}
	// date_i18n(): WordPress' default format in the e-mail's language ("1 Kasım 2026").
	return gooEmailInfo(ctx).cat.Text(wp.FormatDate(t.In(s.loc()), f))
}

// gooEmailFromName / gooEmailFromAddress are the store's sender (Settings › Emails).
func (s *Server) gooEmailFromName() string {
	if v := strings.TrimSpace(s.gooOptString("email_from_name")); v != "" {
		return v
	}
	return s.settings.String("blogname")
}

func (s *Server) gooEmailFromAddress() string {
	if v := strings.TrimSpace(s.gooOptString("email_from_address")); v != "" {
		return v
	}
	return s.settings.String("admin_email")
}

// gooEmailRecipients splits a comma-separated recipient list, keeping valid addresses.
func gooEmailRecipients(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if a, err := mail.ParseAddress(p); err == nil {
			out = append(out, a.Address)
		}
	}
	return out
}

// gooEmail is a rendered e-mail.
type gooEmail struct {
	ID          string
	To          []string
	Subject     string
	Heading     string
	HTML, Plain string
	ReplyTo     string
}

// gooSendEmail renders and queues an e-mail; sent reports whether it was queued (enabled, with a recipient).
func (s *Server) gooSendEmail(ctx context.Context, id string, o *gooOrder, args map[string]any) (bool, error) {
	d := gooEmailDefByID(id)
	if d == nil {
		return false, errors.New("unknown e-mail " + id)
	}
	if !s.gooOn() || d.Module != "" && !s.gooExtOn(d.Module) {
		return false, nil
	}
	st := s.gooEmailSettings(id)
	if !d.Manual && st["enabled"] != "yes" {
		return false, nil
	}
	e, err := s.gooRenderEmail(ctx, d, st, o, args)
	if err != nil {
		return false, err
	}
	if len(e.To) == 0 {
		return false, nil
	}
	from := (&mail.Address{Name: s.gooEmailFromName(), Address: s.gooEmailFromAddress()}).String()
	msg := wpmail.Message{Kind: "goo_" + id, To: e.To, Subject: e.Subject, Body: e.Plain, ReplyTo: e.ReplyTo}
	if _, err := mail.ParseAddress(s.gooEmailFromAddress()); err == nil {
		msg.From = from
	}
	if st["email_type"] != "plain" {
		msg.HTML = e.HTML
	}
	for _, h := range gooEmailAttachHooks {
		for name, data := range h(s, ctx, id, o) {
			ct := mime.TypeByExtension(filepath.Ext(name))
			if ct == "" {
				ct = "application/octet-stream"
			}
			msg.Attachments = append(msg.Attachments, wpmail.Attachment{Name: name, ContentType: ct, Data: data})
		}
	}
	s.sendMail(ctx, msg)
	return true, nil
}

// gooRenderEmail builds an e-mail's recipients, subject and bodies.
func (s *Server) gooRenderEmail(ctx context.Context, d *gooEmailDef, st map[string]string, o *gooOrder, args map[string]any) (*gooEmail, error) {
	if args == nil {
		args = map[string]any{}
	}
	e := &gooEmail{ID: d.ID}
	ctx = s.gooPllOrderCtx(ctx, o) // Golylang: the language the order was placed in (goo_golylang.go)
	switch {
	case gooStockEmails[d.ID]:
		e.To = gooEmailRecipients(s.gooOptString("stock_email_recipient"))
		if len(e.To) == 0 {
			e.To = gooEmailRecipients(st["recipient"])
		}
		if len(e.To) == 0 {
			e.To = gooEmailRecipients(s.settings.String("admin_email"))
		}
	case d.Customer:
		to, _ := args["to"].(string)
		if to == "" && o != nil {
			to = o.Billing["email"]
		}
		if to == "" {
			if uid := gooInt(args["user_id"]); uid != 0 {
				if u, err := s.store.UserByID(ctx, uid); err == nil {
					to = u.Email
				}
			}
		}
		e.To = gooEmailRecipients(to)
	default:
		e.To = gooEmailRecipients(st["recipient"])
		if len(e.To) == 0 {
			e.To = gooEmailRecipients(s.settings.String("admin_email"))
		}
		if o != nil && o.Billing["email"] != "" && (d.ID == "new_order" || d.ID == "cancelled_order" || d.ID == "failed_order") {
			e.ReplyTo = (&mail.Address{Name: o.BillingName(), Address: o.Billing["email"]}).String()
		}
	}
	ctx = s.gooEmailL10n(ctx, d, e.To)
	ph := s.gooEmailPlaceholders(ctx, o)
	subject, heading := st["subject"], st["heading"]
	switch d.ID {
	case "customer_refunded_order":
		if gooBool(args["partial"]) {
			subject = defaultStr(st["subject_partial"], "Your {site_title} order #{order_number} has been partially refunded")
			heading = defaultStr(st["heading_partial"], "Partial Refund: Order {order_number}")
		} else {
			subject = defaultStr(st["subject_full"], subject)
			heading = defaultStr(st["heading_full"], heading)
		}
	case "customer_invoice":
		if o != nil && o.IsPaid() {
			subject = defaultStr(st["subject_paid"], "Your latest {site_title} invoice")
			heading = defaultStr(st["heading_paid"], "Your invoice for order #{order_number}")
		}
	}
	if subject == "" {
		subject = d.Subject
	}
	if heading == "" {
		heading = d.Heading
	}
	e.Subject = gooReplacePlaceholders(s.gooEmailText(ctx, subject), ph)
	e.Heading = gooReplacePlaceholders(s.gooEmailText(ctx, heading), ph)
	// The additional content is HTML written by the shop; the values put into it (the customer's
	// name among them) are not, so they are escaped there. The plain part strips the tags again.
	phHTML := make(map[string]string, len(ph))
	for k, v := range ph {
		phHTML[k] = html.EscapeString(v)
	}
	additional := gooReplacePlaceholders(s.gooEmailText(ctx, st["additional_content"]), phHTML)
	content, plain := s.gooEmailContent(ctx, d, o, args)
	e.HTML = s.gooEmailWrap(ctx, e.Heading, content+gooAutoP(additional))
	e.Plain = "= " + e.Heading + " =\n\n" + plain
	if strings.TrimSpace(additional) != "" {
		e.Plain += "\n\n----------------------------------------\n\n" + gooPlainMsg(additional)
	}
	e.Plain += "\n\n----------------------------------------\n\n" + gooPlainMsg(s.gooEmailFooter(ctx)) + "\n"
	return e, nil
}

// E-mails in the recipient's language, as WooCommerce sends them: a customer's e-mail in the language
// the order was placed in (Pursae Multi Language) or the site language (WC_Email::setup_locale()
// switches to the site locale), a store e-mail in the language of the user it goes to (their profile
// language) or the site's. The texts come from the WooCommerce pack the site language downloads and
// Pursae's own strings; the shop owner's own subject, heading and additional content are used as
// written (or as translated under Languages › Translations).

type gooEmailL10nKey struct{}

type gooEmailL10nInfo struct {
	locale string
	cat    *i18n.Catalog
}

// gooEmailL10n returns ctx carrying the catalog of an e-mail's language.
func (s *Server) gooEmailL10n(ctx context.Context, d *gooEmailDef, to []string) context.Context {
	locale := s.siteLocale()
	if d.Customer {
		locale = s.gooFrontLocale(ctx)
	} else if len(to) > 0 {
		if u, err := s.store.UserByLoginOrEmail(ctx, to[0]); err == nil && strings.EqualFold(u.Email, to[0]) {
			if l, _ := u.Meta["locale"].(string); l != "" {
				locale = l
			}
		}
	}
	info := &gooEmailL10nInfo{locale: locale, cat: &i18n.Catalog{}}
	if s.lang != nil {
		info.cat = s.lang.Catalog(locale)
	}
	return context.WithValue(ctx, gooEmailL10nKey{}, info)
}

func gooEmailInfo(ctx context.Context) *gooEmailL10nInfo {
	if v, ok := ctx.Value(gooEmailL10nKey{}).(*gooEmailL10nInfo); ok {
		return v
	}
	return &gooEmailL10nInfo{locale: "en_US", cat: &i18n.Catalog{}}
}

// gooET is sprintf( __( format ), args… ) in the e-mail's language; args are written as given
// (escaped for HTML by the caller where needed).
func gooET(ctx context.Context, format string, args ...string) string {
	if tr, ok := gooEmailInfo(ctx).cat.Lookup(format); ok {
		format = tr
	}
	if len(args) == 0 {
		return format
	}
	return i18n.Sprintf(format, args...)
}

// gooEmailText is a subject, heading or additional content: the shop owner's translation (Pursae
// Multi Language strings) first, else the catalog's for the default texts.
func (s *Server) gooEmailText(ctx context.Context, v string) string {
	if t := s.gooT(ctx, v); t != v {
		return t
	}
	if strings.TrimSpace(v) == "" {
		return v
	}
	return gooET(ctx, v)
}

// gooEmailResetURL makes a password reset link for a user (get_password_reset_key()).
func (s *Server) gooEmailResetURL(ctx context.Context, userID int64) string {
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return s.gooAccountURL(ctx, "lost-password")
	}
	key := auth.RandomToken(20)
	sum := sha256.Sum256([]byte(key))
	raw, _ := json.Marshal(map[string]any{"password_reset": map[string]any{"hash": hex.EncodeToString(sum[:]), "expires": time.Now().Add(24 * time.Hour).Unix()}})
	if _, err := s.store.Pool.Exec(ctx, `UPDATE users SET meta = coalesce(meta, '{}'::jsonb) || $2::jsonb WHERE id = $1`, u.ID, raw); err != nil {
		return s.gooAccountURL(ctx, "lost-password")
	}
	// WooCommerce links to My Account › Lost password with the key (the same key checkResetKey reads),
	// which also keeps a hidden login address out of the email.
	if s.gooPageID("myaccount") > 0 {
		return gooAddQuery(s.gooAccountURL(withConfiguredOrigin(ctx), "lost-password"), "key="+key+"&login="+url.QueryEscape(u.Login))
	}
	return s.configuredLinks(ctx).HomeURL(s.loginPath() + "?action=rp&key=" + key + "&login=" + url.QueryEscape(u.Login))
}

// gooEmailPreview renders an e-mail with a sample order for the settings screen's preview.
func (s *Server) gooEmailPreview(id string) (subject, html string) {
	ctx := context.Background()
	d := gooEmailDefByID(id)
	if d == nil {
		return "", ""
	}
	now := time.Now()
	addr := map[string]string{"first_name": "John", "last_name": "Doe", "company": "Company", "address_1": "123 Sample Street", "city": "Los Angeles",
		"state": "CA", "postcode": "90210", "country": "US"}
	billing := map[string]string{"email": "john@example.com", "phone": "555-555-5555"}
	for k, v := range addr {
		billing[k] = v
	}
	o := &gooOrder{ID: 12345, Type: "shop_order", Status: "processing", Currency: s.gooCurrency(), Billing: billing, Shipping: addr,
		PaymentMethod: "bacs", PaymentMethodTitle: "Direct bank transfer", CustomerNote: "This is a sample customer note.",
		DateCreated: now, DatePaid: &now, Meta: map[string]any{"_goo_preview": true},
		Items: []*gooOrderItem{
			{ID: 1, Type: "line_item", Name: "Dummy Product", Quantity: 2, Subtotal: 20, Total: 20, Meta: map[string]any{}},
			{ID: 2, Type: "line_item", Name: "Dummy Product Variation", Quantity: 1, Subtotal: 15, Total: 15,
				Meta: map[string]any{"_display": []any{map[string]any{"key": "Color", "value": "Red"}, map[string]any{"key": "Size", "value": "Small"}}}},
			{ID: 3, Type: "shipping", Name: "Flat rate", Total: 5, Meta: map[string]any{"method_id": "flat_rate"}},
		}}
	o.ShippingTotal, o.Total = 5, 40
	args := map[string]any{"preview": true, "note": "This is a sample customer note.", "partial": false, "login": "johndoe", "to": "john@example.com",
		"password_generated": true, "reset_url": s.links(ctx).HomeURL(s.loginPath() + "?action=rp&key=sample&login=johndoe"),
		"product": &gooProduct{ID: 1, Name: "Dummy Product", SKU: "DUMMY-1", StockQuantity: gooIntPtr(1)}, "quantity": 2}
	if !strings.HasPrefix(id, "customer_new_account") && id != "customer_reset_password" {
		args["to"] = ""
	}
	e, err := s.gooRenderEmail(ctx, d, s.gooEmailSettings(id), o, args)
	if err != nil {
		return "", ""
	}
	return e.Subject, e.HTML
}
