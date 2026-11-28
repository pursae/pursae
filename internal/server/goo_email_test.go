package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gooMails are the queued wp_mail jobs (mail.Message payloads), oldest first.
func gooMails(t *testing.T, srv *Server) []map[string]any {
	t.Helper()
	rows, err := srv.store.Pool.Query(context.Background(), `SELECT payload FROM jobs WHERE name = 'wp_mail' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if rows.Scan(&raw) == nil {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			out = append(out, m)
		}
	}
	return out
}

// gooMailsWith filters the queued mails whose subject contains a text.
func gooMailsWith(t *testing.T, srv *Server, subject string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range gooMails(t, srv) {
		if strings.Contains(gooStr(m["Subject"]), subject) {
			out = append(out, m)
		}
	}
	return out
}

func gooMailTo(m map[string]any) string {
	l, _ := m["To"].([]any)
	var out []string
	for _, x := range l {
		out = append(out, gooStr(x))
	}
	return strings.Join(out, ",")
}

// gooTestOrder makes a pending order for a product with a customer billing address.
func gooTestOrder(t *testing.T, srv *Server, p *gooProduct, qty int) *gooOrder {
	t.Helper()
	billing := map[string]string{}
	for k, v := range gooTestAddress {
		billing[k] = v.(string)
	}
	o := &gooOrder{Status: "pending", Billing: billing, Shipping: map[string]string{}, PaymentMethod: "bacs", PaymentMethodTitle: "Direct bank transfer",
		Items: []*gooOrderItem{{Type: "line_item", Name: p.Name, ProductID: p.ID, Quantity: float64(qty), Subtotal: 10 * float64(qty), Total: 10 * float64(qty), Meta: map[string]any{}}},
		Meta:  map[string]any{}}
	srv.gooRecalculate(o)
	if err := srv.gooSaveOrder(context.Background(), o, true); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestGooEmailOrderProcessing(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	p := gooMakeSimple(t, srv, "Teapot", 10, func(p *gooProduct) { p.SKU = "TEA-1" })
	o := gooTestOrder(t, srv, p, 2)
	if err := srv.gooSetStatus(ctx, o, "processing", "", ""); err != nil {
		t.Fatal(err)
	}
	num := o.Number()
	admin := gooMailsWith(t, srv, "[Toggle Site]: New order #"+num)
	if len(admin) != 1 || gooMailTo(admin[0]) != "admin@example.test" {
		t.Fatalf("new_order mails: %v", admin)
	}
	html := gooStr(admin[0]["HTML"])
	for _, want := range []string{"New Order: #" + num, "Teapot (#TEA-1)", "Billing address", "Ann Lee", "Subtotal:", "Total:", "Direct bank transfer", "background-color:#7f54b3"} {
		if !strings.Contains(html, want) {
			t.Errorf("new_order HTML lacks %q", want)
		}
	}
	if !strings.Contains(gooStr(admin[0]["ReplyTo"]), "ann@example.test") {
		t.Errorf("new_order Reply-To: %v", admin[0]["ReplyTo"])
	}
	cust := gooMailsWith(t, srv, "Your Toggle Site order has been received!")
	if len(cust) != 1 || gooMailTo(cust[0]) != "ann@example.test" {
		t.Fatalf("customer_processing_order mails: %v", cust)
	}
	if body := gooStr(cust[0]["Body"]); !strings.Contains(body, "Hi Ann,") || !strings.Contains(body, "Teapot (#TEA-1) × 2") || strings.Contains(body, "<td") {
		t.Errorf("plain part: %s", body)
	}
	if !strings.Contains(gooStr(cust[0]["From"]), "admin@example.test") {
		t.Errorf("From: %v", cust[0]["From"])
	}
	var sent bool
	_ = srv.store.Pool.QueryRow(ctx, `SELECT new_order_email_sent FROM goo_orders WHERE id = $1`, o.ID).Scan(&sent)
	if !sent {
		t.Error("new_order_email_sent not recorded")
	}
	// Back to pending and paid again: the admin e-mail is not sent twice.
	_ = srv.gooSetStatus(ctx, o, "pending", "", "")
	_ = srv.gooSetStatus(ctx, o, "processing", "", "")
	if n := len(gooMailsWith(t, srv, "New order #"+num)); n != 1 {
		t.Errorf("new_order sent %d times", n)
	}
	for _, m := range gooMails(t, srv) {
		if s := gooStr(m["Subject"]); strings.Contains(s, "{") {
			t.Errorf("placeholder left in %q", s)
		}
	}
	// Completed, then cancelled from processing (admin).
	_ = srv.gooSetStatus(ctx, o, "completed", "", "")
	if len(gooMailsWith(t, srv, "Your Toggle Site order is now complete")) != 1 {
		t.Error("no customer_completed_order")
	}
}

func TestGooEmailRefundDisabledPlain(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	p := gooMakeSimple(t, srv, "Kettle", 10, nil)
	if err := srv.gooSetOpts(ctx, map[string]any{
		"email_customer_completed_order_settings": map[string]any{"enabled": "no"},
		"email_new_order_settings":                map[string]any{"email_type": "plain", "subject": "Order {order_number} at {site_title} on {order_date}", "recipient": "boss@example.test, ops@example.test"},
		"email_from_name":                         "Kettle Shop", "email_from_address": "shop@example.test",
	}); err != nil {
		t.Fatal(err)
	}
	o := gooTestOrder(t, srv, p, 3)
	_ = srv.gooSetStatus(ctx, o, "processing", "", "")
	plain := gooMailsWith(t, srv, "Order "+o.Number()+" at Toggle Site on ")
	if len(plain) != 1 {
		t.Fatalf("custom subject: %v", gooMails(t, srv))
	}
	if gooStr(plain[0]["HTML"]) != "" || !strings.Contains(gooStr(plain[0]["Body"]), "= New Order: #"+o.Number()+" =") {
		t.Errorf("plain e-mail: HTML=%q Body=%q", plain[0]["HTML"], plain[0]["Body"])
	}
	if gooMailTo(plain[0]) != "boss@example.test,ops@example.test" || !strings.Contains(gooStr(plain[0]["From"]), "Kettle Shop") {
		t.Errorf("recipients / From: %s %v", gooMailTo(plain[0]), plain[0]["From"])
	}
	// Partial then full refund.
	if _, err := srv.gooCreateRefund(ctx, o, 10, "Broken", nil, false, false, 1); err != nil {
		t.Fatal(err)
	}
	part := gooMailsWith(t, srv, "has been partially refunded")
	if len(part) != 1 || !strings.Contains(gooStr(part[0]["HTML"]), "Partial Refund: Order "+o.Number()) || !strings.Contains(gooStr(part[0]["HTML"]), "Refund:") {
		t.Fatalf("partial refund mail: %v", part)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	if _, err := srv.gooCreateRefund(ctx, o, 20, "", nil, false, false, 1); err != nil {
		t.Fatal(err)
	}
	if len(gooMailsWith(t, srv, "Your Toggle Site order #"+o.Number()+" has been refunded")) != 1 {
		t.Error("no full refund mail")
	}
	// A disabled e-mail is not sent.
	o2 := gooTestOrder(t, srv, p, 1)
	_ = srv.gooSetStatus(ctx, o2, "completed", "", "")
	if n := len(gooMailsWith(t, srv, "is now complete")); n != 0 {
		t.Errorf("disabled customer_completed_order sent %d times", n)
	}
	// Customer note and the manual invoice.
	_ = srv.gooAddOrderNote(ctx, o2, "Your parcel left today.", true, "admin")
	if m := gooMailsWith(t, srv, "Note added to your Toggle Site order from"); len(m) != 1 || !strings.Contains(gooStr(m[0]["HTML"]), "Your parcel left today.") {
		t.Errorf("customer note mail: %v", m)
	}
	if err := srv.gooTriggerEmail(ctx, "customer_invoice", o2, nil); err != nil {
		t.Fatal(err)
	}
	if len(gooMailsWith(t, srv, "Your latest Toggle Site invoice")) != 1 {
		t.Error("no invoice mail")
	}
}

func TestGooEmailAccountsAndStock(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	if _, err := srv.gooCreateCustomer(ctx, "new@example.test", "", "", map[string]string{"billing_first_name": "Neo"}); err != nil {
		t.Fatal(err)
	}
	m := gooMailsWith(t, srv, "Your Toggle Site account has been created!")
	if len(m) != 1 || gooMailTo(m[0]) != "new@example.test" || !strings.Contains(gooStr(m[0]["HTML"]), "/my-account/lost-password/?key=") {
		t.Fatalf("new account mail: %v", m)
	}
	// Stock notices go to the stock recipient.
	_ = srv.gooSetOpts(ctx, map[string]any{"stock_email_recipient": "stock@example.test"})
	p := gooMakeSimple(t, srv, "Vase", 10, func(p *gooProduct) { p.ManageStock, p.StockQuantity = true, gooIntPtr(3) })
	o := gooTestOrder(t, srv, p, 2)
	_ = srv.gooSetStatus(ctx, o, "processing", "", "")
	low := gooMailsWith(t, srv, "Product low in stock")
	if len(low) != 1 || gooMailTo(low[0]) != "stock@example.test" || !strings.Contains(gooStr(low[0]["Body"]), "Vase is low in stock. There are 1 left.") {
		t.Errorf("low stock mail: %v", low)
	}
	// Preview for the settings screen.
	subj, html := srv.gooEmailPreview("customer_processing_order")
	if subj != "Your Toggle Site order has been received!" || !strings.Contains(html, "Dummy Product") || !strings.Contains(html, "Color:") {
		t.Errorf("preview: %q %d", subj, len(html))
	}
}

// TestGooEmailTurkish: with Turkish as the site language the store's e-mails are Turkish, as
// WooCommerce sends them with its language pack (here a small one): the customer's order e-mail
// (subject, heading, greeting, order table, addresses, footer) and the store's new order e-mail. A
// store e-mail goes out in the language of the user it is addressed to when they chose one in their
// profile. The product's name stays as the shop owner wrote it.
func TestGooEmailTurkish(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	dir := srv.lang.Dir
	if err := os.MkdirAll(filepath.Join(dir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := "Plural-Forms: nplurals=2; plural=(n > 1);\n"
	if err := os.WriteFile(filepath.Join(dir, "admin-tr_TR.mo"), i18nMO(map[string]string{"": head, "F j, Y": "j F Y", "November": "Kasım"}), 0o644); err != nil {
		t.Fatal(err)
	}
	woo := i18nMO(map[string]string{"": head,
		"Your {site_title} order has been received!": "{site_title} siparişiniz alındı!", "Thank you for your order": "Siparişiniz için teşekkürler",
		"Hi %s,": "Merhaba %s,", "Just to let you know &mdash; we've received your order #%s, and it is now being processed:": "Bilginize &mdash; #%s numaralı siparişinizi aldık ve şu anda işleniyor:",
		"[Order #%s]": "[Sipariş #%s]", "Product": "Ürün", "Quantity": "Miktar", "Price": "Fiyat", "Subtotal:": "Ara toplam:", "Total:": "Toplam:",
		"Payment method:": "Ödeme yöntemi:", "Billing address": "Fatura adresi", "Thanks for using {site_url}!": "{site_url} kullandığınız için teşekkürler!",
		"[{site_title}]: New order #{order_number}": "[{site_title}]: Yeni sipariş #{order_number}", "New Order: #{order_number}": "Yeni sipariş: #{order_number}",
		"You’ve received the following order from %s:": "%s adlı kişiden şu siparişi aldınız:",
		"Teapot": "YANLIŞ: ürün adı çevrildi", // the product's name must stay as written
	})
	if err := os.WriteFile(filepath.Join(dir, "plugins", "woocommerce-tr_TR.mo"), woo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.settings.Set(ctx, map[string]any{"WPLANG": "tr_TR"}); err != nil {
		t.Fatal(err)
	}
	srv.lang.Reset()

	p := gooMakeSimple(t, srv, "Teapot", 10, nil)
	o := gooTestOrder(t, srv, p, 2)
	if err := srv.gooSetStatus(ctx, o, "processing", "", ""); err != nil {
		t.Fatal(err)
	}
	num := o.Number()
	cust := gooMailsWith(t, srv, "Toggle Site siparişiniz alındı!")
	if len(cust) != 1 || gooMailTo(cust[0]) != "ann@example.test" {
		t.Fatalf("Turkish customer_processing_order: %v", gooMails(t, srv))
	}
	html, body := gooStr(cust[0]["HTML"]), gooStr(cust[0]["Body"])
	for _, want := range []string{`<html lang="tr-TR">`, "Siparişiniz için teşekkürler", "Merhaba Ann,", "Bilginize &mdash; #" + num + " numaralı siparişinizi aldık",
		"[Sipariş #" + num + "]", ">Ürün<", ">Miktar<", ">Fiyat<", "Ara toplam:", "Toplam:", "Ödeme yöntemi:", "Fatura adresi",
		"Teapot", "Pursae ile oluşturuldu", // Pursae's own footer text (web/languages/wpgo-tr_TR.json)
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Turkish e-mail lacks %q", want)
		}
	}
	for _, unwanted := range []string{"YANLIŞ", "Thank you for your order", "Billing address", "Built with Pursae"} {
		if strings.Contains(html, unwanted) || strings.Contains(body, unwanted) {
			t.Errorf("Turkish e-mail has %q", unwanted)
		}
	}
	if !strings.Contains(body, "Merhaba Ann,") || !strings.Contains(body, "Bilginize — #"+num) || !strings.Contains(body, "Fatura adresi") {
		t.Errorf("Turkish plain part: %s", body)
	}
	if admin := gooMailsWith(t, srv, "[Toggle Site]: Yeni sipariş #"+num); len(admin) != 1 || !strings.Contains(gooStr(admin[0]["HTML"]), "Ann Lee adlı kişiden şu siparişi aldınız:") {
		t.Errorf("Turkish new_order: %v", gooMails(t, srv))
	}

	// The administrator reads English (their profile language): the store's e-mail to them is English.
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE users SET meta = meta || '{"locale": "en_US"}' WHERE email = 'admin@example.test'`); err != nil {
		t.Fatal(err)
	}
	o2 := gooTestOrder(t, srv, p, 1)
	_ = srv.gooSetStatus(ctx, o2, "processing", "", "")
	if len(gooMailsWith(t, srv, "[Toggle Site]: New order #"+o2.Number())) != 1 {
		t.Errorf("new_order to an English-speaking administrator: %v", gooMails(t, srv))
	}
}
