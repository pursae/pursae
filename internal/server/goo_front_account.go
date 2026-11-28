package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/auth"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// My Account (my-account.php and its endpoints), the login / registration / address / account forms,
// customer downloads with the download handler, and order tracking.

// gooLoginFormHTML is form-login.php (toggle: the checkout's hidden login form).
func (s *Server) gooLoginFormHTML(c *gin.Context, redirect string, toggle bool) string {
	return s.gooTpl("goo/form-login", map[string]any{
		"Action": s.gooPageURL(c.Request.Context(), "myaccount"), "Toggle": toggle, "Username": c.PostForm("username"),
		"Nonce": s.nonce(c, "woocommerce-login"), "Redirect": redirect, "LostURL": s.gooAccountURL(c.Request.Context(), "lost-password"),
		"Captcha": string(s.gooLoginCaptchaWidget(c)), // Pursae Security: CAPTCHA on the account forms
	})
}

// gooLoginCaptchaKey marks a session whose last sign-in was asked for the "account under attack" CAPTCHA.
const gooLoginCaptchaKey = "login_captcha"

// gooLoginCaptchaWidget is the login form's CAPTCHA: the always-on one, or the one loginPressureCaptcha
// asked for on this session's last attempt.
func (s *Server) gooLoginCaptchaWidget(c *gin.Context) template.HTML {
	if w := s.loginCaptchaWidget(c); w != "" {
		return w
	}
	if asked, _ := s.gooSessionFor(c).Data.Extra[gooLoginCaptchaKey].(bool); asked {
		if p, ok := s.captchaProvider(); ok {
			return captchaWidget(c, p, "login")
		}
	}
	return ""
}

// gooAccountPageHTML is the [goocommerce_my_account] shortcode.
func (s *Server) gooAccountPageHTML(c *gin.Context) string {
	ctx := c.Request.Context()
	u := currentUser(c)
	var b strings.Builder
	b.WriteString(`<div class="woocommerce">`)
	if fv := gooViewFrom(c); fv != nil && fv.Kind == "myaccount" && fv.Endpoint == "lost-password" {
		b.WriteString(s.gooLostPasswordHTML(c))
		b.WriteString(`</div>`)
		s.gooSaveSession(c)
		return b.String()
	}
	if u == nil {
		b.WriteString(s.gooNoticesHTML(c))
		reg := s.gooOptBool("enable_myaccount_registration")
		privacy := ""
		if t := strings.TrimSpace(s.gooOptString("registration_privacy_policy_text")); t != "" {
			privacy = wp.Autop(s.gooPrivacyText(c, t))
		}
		b.WriteString(s.gooTpl("goo/myaccount-login", map[string]any{
			"Register": reg, "LoginForm": s.gooLoginFormHTML(c, "", false), "Action": s.gooPageURL(ctx, "myaccount"),
			"GenUsername": s.gooOptBool("registration_generate_username"), "GenPassword": s.gooOptBool("registration_generate_password"),
			"RegUsername": c.PostForm("username"), "RegEmail": c.PostForm("email"), "Privacy": privacy, "RegNonce": s.nonce(c, "woocommerce-register"),
			"Captcha": string(s.loginCaptchaWidget(c)),
		}))
		b.WriteString(`</div>`)
		s.gooSaveSession(c)
		return b.String()
	}
	fv := gooViewFrom(c)
	endpoint, value := "", ""
	if fv != nil && fv.Kind == "myaccount" {
		endpoint, value = fv.Endpoint, fv.Value
	}
	b.WriteString(s.gooAccountNav(c, endpoint))
	b.WriteString(`<div class="woocommerce-MyAccount-content">`)
	b.WriteString(s.gooNoticesHTML(c))
	switch endpoint {
	case "":
		b.WriteString(s.gooAccountDashboard(c, u))
	case "orders":
		b.WriteString(s.gooAccountOrders(c, u, max(gooAtoi(value), 1)))
	case "view-order":
		b.WriteString(s.gooAccountViewOrder(c, u, gooAtoi64(value)))
	case "downloads":
		dl := s.gooCustomerDownloads(c, u)
		if len(dl) == 0 {
			b.WriteString(`<div class="woocommerce-Message woocommerce-Message--info woocommerce-info">` + s.gooBrowseButton(c) + `No downloads available yet.</div>`)
		} else {
			b.WriteString(`<section class="woocommerce-order-downloads">` + s.gooDownloadsTable(c, dl) + `</section>`)
		}
	case "edit-address":
		b.WriteString(s.gooAccountAddresses(c, u, value))
	case "edit-account":
		b.WriteString(s.gooTpl("goo/form-edit-account", map[string]any{
			"Action": s.gooAccountURL(ctx, "edit-account"), "First": u.Profile["first_name"], "Last": u.Profile["last_name"],
			"Display": u.DisplayName, "Email": u.Email, "Nonce": s.nonce(c, "save_account_details"),
		}))
	case "payment-methods":
		b.WriteString(s.gooPaymentMethodsHTML(c))
	default:
		for _, ep := range s.gooModuleEndpoints() {
			if ep.Slug == endpoint && ep.Render != nil {
				b.WriteString(string(ep.Render(s, c, value)))
			}
		}
	}
	b.WriteString(`</div></div>`)
	s.gooSaveSession(c)
	return b.String()
}

func (s *Server) gooBrowseButton(c *gin.Context) string {
	return `<a class="woocommerce-Button wc-forward button" href="` + gooEsc(s.gooShopURL(c.Request.Context())) + `">Browse products</a>`
}

type gooNavItem struct {
	Key, Title, URL string
	Active          bool
}

// gooAccountNav is navigation.php with the modules' endpoints after their After item.
func (s *Server) gooAccountNav(c *gin.Context, current string) string {
	ctx := c.Request.Context()
	base := []gooNavItem{{Key: "dashboard", Title: "Dashboard"}, {Key: "orders", Title: "Orders"}, {Key: "downloads", Title: "Downloads"},
		{Key: "edit-address", Title: "Addresses"}, {Key: "edit-account", Title: "Account details"}, {Key: "customer-logout", Title: "Log out"}}
	if len(s.gooTokenGateways()) > 0 { // WooCommerce: the tab shows when a gateway saves payment methods
		base = append(base[:4], append([]gooNavItem{{Key: "payment-methods", Title: "Payment methods"}}, base[4:]...)...)
	}
	items := []gooNavItem{}
	// An endpoint without a title (view-subscription) is a detail page, not a tab: it lights up
	// the tab it follows, the way view-order lights up Orders.
	var mods []*gooAccountEndpoint
	parent := map[string]string{"view-order": "orders"}
	for _, ep := range s.gooModuleEndpoints() {
		if ep.Title == "" {
			parent[ep.Slug] = ep.After
			continue
		}
		mods = append(mods, ep)
	}
	placed := map[string]bool{}
	for _, it := range base {
		items = append(items, it)
		for _, ep := range mods {
			if ep.After == it.Key && !placed[ep.Slug] {
				items = append(items, gooNavItem{Key: ep.Slug, Title: ep.Title})
				placed[ep.Slug] = true
			}
		}
	}
	for _, ep := range mods { // unknown After: before Log out
		if !placed[ep.Slug] {
			n := len(items) - 1
			items = append(items[:n], append([]gooNavItem{{Key: ep.Slug, Title: ep.Title}}, items[n:]...)...)
		}
	}
	for i := range items {
		it := &items[i]
		switch it.Key {
		case "dashboard":
			it.URL = s.gooPageURL(ctx, "myaccount")
			it.Active = current == ""
		case "customer-logout":
			it.URL = s.gooLogoutURL(c)
		default:
			it.URL = s.gooEndpointURL(ctx, "myaccount", s.gooEndpointSlug(it.Key), "")
			it.Active = current == it.Key || parent[current] == it.Key
		}
	}
	return s.gooTpl("goo/myaccount-navigation", items)
}

func (s *Server) gooLogoutURL(c *gin.Context) string {
	return s.gooEndpointURL(c.Request.Context(), "myaccount", s.gooEndpointSlug("customer-logout"), "") + "?_wpnonce=" + s.nonce(c, "customer-logout")
}

// gooAccountDashboard is dashboard.php.
func (s *Server) gooAccountDashboard(c *gin.Context, u *store.User) string {
	ctx := c.Request.Context()
	name := gooEsc(u.DisplayName)
	return `<p>Hello <strong>` + name + `</strong> (not <strong>` + name + `</strong>? <a href="` + gooEsc(s.gooLogoutURL(c)) + `">Log out</a>)</p>` +
		`<p>From your account dashboard you can view your <a href="` + gooEsc(s.gooAccountURL(ctx, "orders")) + `">recent orders</a>, manage your <a href="` +
		gooEsc(s.gooAccountURL(ctx, "edit-address")) + `">shipping and billing addresses</a>, and <a href="` + gooEsc(s.gooAccountURL(ctx, "edit-account")) +
		`">edit your password and account details</a>.</p>`
}

// gooAccountOrders is orders.php.
func (s *Server) gooAccountOrders(c *gin.Context, u *store.User, page int) string {
	ctx := c.Request.Context()
	const per = 10
	orders, total, _ := s.gooQueryOrders(ctx, gooOrderQuery{CustomerID: u.ID, Page: page, PerPage: per, OrderBy: "date", Order: "desc"})
	if total == 0 {
		return `<div class="woocommerce-message woocommerce-message--info woocommerce-Message woocommerce-Message--info woocommerce-info">` + s.gooBrowseButton(c) + `No order has been made yet.</div>`
	}
	var b strings.Builder
	b.WriteString(`<table class="woocommerce-orders-table woocommerce-MyAccount-orders shop_table shop_table_responsive my_account_orders account-orders-table"><thead><tr>`)
	cols := [][2]string{{"order-number", "Order"}, {"order-date", "Date"}, {"order-status", "Status"}, {"order-total", "Total"}, {"order-actions", "Actions"}}
	for _, col := range cols {
		b.WriteString(`<th class="woocommerce-orders-table__header woocommerce-orders-table__header-` + col[0] + `"><span class="nobr">` + col[1] + `</span></th>`)
	}
	b.WriteString(`</tr></thead><tbody>`)
	for _, o := range orders {
		view := s.gooEndpointURL(ctx, "myaccount", s.gooEndpointSlug("view-order"), itoa64(o.ID))
		b.WriteString(`<tr class="woocommerce-orders-table__row woocommerce-orders-table__row--status-` + gooEsc(o.Status) + ` order">`)
		b.WriteString(`<th class="woocommerce-orders-table__cell woocommerce-orders-table__cell-order-number" data-title="Order" scope="row"><a href="` + gooEsc(view) + `" aria-label="View order number ` + gooEsc(o.Number()) + `">#` + gooEsc(o.Number()) + `</a></th>`)
		t := o.DateCreated.In(s.loc())
		b.WriteString(`<td class="woocommerce-orders-table__cell woocommerce-orders-table__cell-order-date" data-title="Date"><time datetime="` + t.Format(time.RFC3339) + `">` + wp.FormatDate(t, s.settings.String("date_format")) + `</time></td>`)
		b.WriteString(`<td class="woocommerce-orders-table__cell woocommerce-orders-table__cell-order-status" data-title="Status">` + gooEsc(gooStatusLabel(o.Status)) + `</td>`)
		n := o.ItemCount()
		word := "items"
		if n == 1 {
			word = "item"
		}
		b.WriteString(`<td class="woocommerce-orders-table__cell woocommerce-orders-table__cell-order-total" data-title="Total">` + string(s.gooPriceIn(o.Total, o.Currency)) + fmt.Sprintf(` for %d %s`, n, word) + `</td>`)
		b.WriteString(`<td class="woocommerce-orders-table__cell woocommerce-orders-table__cell-order-actions" data-title="Actions">`)
		if o.Status == "pending" || o.Status == "failed" {
			b.WriteString(`<a href="` + gooEsc(s.gooOrderPayURL(ctx, o)) + `" class="woocommerce-button button pay">Pay</a> `)
			cancel := s.gooPageURL(ctx, "cart") + "?cancel_order=true&order=" + o.OrderKey + "&order_id=" + itoa64(o.ID) + "&redirect=" + urlEscape(s.gooAccountURL(ctx, "orders")) + "&_wpnonce=" + s.nonce(c, "woocommerce-cancel_order")
			b.WriteString(`<a href="` + gooEsc(cancel) + `" class="woocommerce-button button cancel">Cancel</a> `)
		}
		b.WriteString(`<a href="` + gooEsc(view) + `" class="woocommerce-button button view" aria-label="View order number ` + gooEsc(o.Number()) + `">View</a></td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	pages := (total + per - 1) / per
	if pages > 1 {
		b.WriteString(`<div class="woocommerce-pagination woocommerce-pagination--without-numbers woocommerce-Pagination">`)
		if page > 1 {
			b.WriteString(`<a class="woocommerce-button woocommerce-button--previous woocommerce-Button woocommerce-Button--previous button" href="` + gooEsc(s.gooEndpointURL(ctx, "myaccount", s.gooEndpointSlug("orders"), strconv.Itoa(page-1))) + `">Previous</a>`)
		}
		if page < pages {
			b.WriteString(`<a class="woocommerce-button woocommerce-button--next woocommerce-Button woocommerce-Button--next button" href="` + gooEsc(s.gooEndpointURL(ctx, "myaccount", s.gooEndpointSlug("orders"), strconv.Itoa(page+1))) + `">Next</a>`)
		}
		b.WriteString(`</div>`)
	}
	return b.String()
}

// gooAccountViewOrder is view-order.php.
func (s *Server) gooAccountViewOrder(c *gin.Context, u *store.User, id int64) string {
	ctx := c.Request.Context()
	o, err := s.gooGetOrder(ctx, id)
	if err != nil || o.Type != "shop_order" || o.CustomerID != u.ID {
		return `<div class="woocommerce-error">Invalid order. <a href="` + gooEsc(s.gooPageURL(ctx, "myaccount")) + `" class="wc-forward">My account</a></div>`
	}
	return s.gooOrderStatusText(o) + s.gooOrderNotesHTML(c, o) + s.gooOrderDetailsHTML(c, o, true)
}

func (s *Server) gooOrderStatusText(o *gooOrder) string {
	return `<p>Order #<mark class="order-number">` + gooEsc(o.Number()) + `</mark> was placed on <mark class="order-date">` +
		wp.FormatDate(o.DateCreated.In(s.loc()), s.settings.String("date_format")) + `</mark> and is currently <mark class="order-status">` + gooEsc(gooStatusLabel(o.Status)) + `</mark>.</p>`
}

// gooOrderNotesHTML lists the customer notes of an order ("Order updates").
func (s *Server) gooOrderNotesHTML(c *gin.Context, o *gooOrder) string {
	notes, _ := s.gooOrderNotes(c.Request.Context(), o.ID, true)
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<h2>Order updates</h2><ol class="woocommerce-OrderUpdates commentlist notes">`)
	for _, n := range notes {
		t := n.CreatedAt.In(s.loc())
		b.WriteString(`<li class="woocommerce-OrderUpdate comment note"><div class="woocommerce-OrderUpdate-inner comment_container"><div class="woocommerce-OrderUpdate-text comment-text">` +
			`<p class="woocommerce-OrderUpdate-meta meta">` + wp.FormatDate(t, "l jS \\o\\f F Y, h:ia") + `</p><div class="woocommerce-OrderUpdate-description description">` +
			wp.Autop(wp.KsesPost(n.Content)) + `</div><div class="clear"></div></div><div class="clear"></div></div></li>`)
	}
	b.WriteString(`</ol>`)
	return b.String()
}

// gooAccountAddresses is my-address.php and form-edit-address.php.
func (s *Server) gooAccountAddresses(c *gin.Context, u *store.User, typ string) string {
	ctx := c.Request.Context()
	if typ == "billing" || typ == "shipping" {
		value := func(k string) string {
			if v, ok := c.Request.PostForm[k]; ok && len(v) > 0 {
				return v[0]
			}
			if v, ok := u.Meta[k].(string); ok {
				return v
			}
			switch k {
			case "billing_email":
				return u.Email
			case typ + "_first_name":
				return u.Profile["first_name"]
			case typ + "_last_name":
				return u.Profile["last_name"]
			case typ + "_country":
				return s.gooBaseCountry()
			}
			return ""
		}
		title := "Billing address"
		if typ == "shipping" {
			title = "Shipping address"
		}
		return `<form method="post" action="` + gooEsc(s.gooEndpointURL(ctx, "myaccount", s.gooEndpointSlug("edit-address"), typ)) + `"><h2>` + title + `</h2>` +
			`<div class="woocommerce-address-fields"><div class="woocommerce-address-fields__field-wrapper">` + s.gooAddressFieldsHTML(c, typ, value) + `</div>` +
			`<p><button type="submit" class="button" name="save_address" value="Save address">Save address</button>` +
			`<input type="hidden" name="woocommerce-edit-address-nonce" value="` + s.nonce(c, "woocommerce-edit_address") + `" /><input type="hidden" name="action" value="edit_address" /><input type="hidden" name="address_type" value="` + typ + `" /></p></div></form>`
	}
	var b strings.Builder
	b.WriteString(`<p>The following addresses will be used on the checkout page by default.</p><div class="u-columns woocommerce-Addresses col2-set addresses">`)
	for i, t := range []string{"billing", "shipping"} {
		if t == "shipping" && !s.gooShippingEnabled() {
			continue
		}
		title := s.gooL10nText(ctx, map[string]string{"billing": "Billing address", "shipping": "Shipping address"}[t]) // also inside "Edit %s"
		addr := map[string]string{}
		for _, k := range gooAddressKeys {
			if v, ok := u.Meta[t+"_"+k].(string); ok {
				addr[k] = v
			}
		}
		formatted := gooFrontFormatAddress(addr)
		if strings.TrimSpace(addr["address_1"]+addr["city"]+addr["first_name"]) == "" {
			formatted = "You have not set up this type of address yet."
		}
		b.WriteString(fmt.Sprintf(`<div class="u-column%d col-%d woocommerce-Address"><header class="woocommerce-Address-title title"><h2>%s</h2><a href="%s" class="edit">Edit %s</a></header><address>%s</address></div>`,
			i+1, i+1, title, gooEsc(s.gooEndpointURL(ctx, "myaccount", s.gooEndpointSlug("edit-address"), t)), title, formatted))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// Account forms -----------------------------------------------------------------------------------------------

// gooLoginPost is WC_Form_Handler::process_login() through the core authentication.
func (s *Server) gooLoginPost(c *gin.Context) {
	ctx := c.Request.Context()
	account := s.gooPageURL(ctx, "myaccount")
	back := account
	if ref := c.Request.Referer(); ref != "" {
		back = s.gooSafeURL(ctx, ref, account)
	}
	fail := func(msg string) {
		s.gooAddNotice(c, "error", msg)
		s.gooRedirect(c, back)
	}
	if !s.checkNonceValue(c, c.PostForm("woocommerce-login-nonce"), "woocommerce-login") {
		fail("<strong>Error:</strong> Your session has expired. Please reload the page and try again.")
		return
	}
	if msg := s.loginCaptchaFailed(c); msg != "" { // Pursae Security: before any account lookup
		fail(string(msg))
		return
	}
	login, pass := strings.TrimSpace(c.PostForm("username")), c.PostForm("password")
	remember := c.PostForm("rememberme") != ""
	if login == "" {
		fail("<strong>Error:</strong> Username is required.")
		return
	}
	if pass == "" {
		fail("<strong>Error:</strong> The password field is empty.")
		return
	}
	ip := clientIP(c)
	secOn := s.adminsecOn()
	if secOn {
		if msg := s.adminsecPreLogin(c, login); msg != "" {
			fail(string(msg))
			return
		}
	}
	// An account under attack asks for the CAPTCHA here as on wp-login; the session remembers it so
	// the login form shows the widget after the redirect.
	if widget, msg := s.loginPressureCaptcha(c, login); widget != "" {
		sess := s.gooSessionFor(c)
		if sess.Data.Extra == nil {
			sess.Data.Extra = map[string]any{}
		}
		sess.Data.Extra[gooLoginCaptchaKey] = true
		sess.touch()
		if msg != "" {
			fail(string(msg))
			return
		}
	}
	// Core throttle, as on wp-login: the attempt is recorded before the password is checked so parallel
	// guesses cannot all pass the count (auth.BeginAttempt).
	var attemptID int64
	attemptKey := login
	if secOn { // Pursae Security: the same count-first rule against its own limits
		id, msg := s.adminsecBeginAttempt(c, login)
		if msg != "" {
			fail(string(msg))
			return
		}
		attemptID = id
	} else {
		id, key, throttled := s.coreBeginAttempt(c, login)
		if throttled {
			fail("<strong>Error:</strong> Too many failed login attempts. Please try again in 15 minutes.")
			return
		}
		attemptID, attemptKey = id, key
	}
	u, err := s.store.UserByLoginOrEmail(ctx, login)
	ok := false
	if err == nil {
		ok = s.checkUserPassword(c.Request.Context(), u, pass)
	} else {
		auth.CheckDummyPassword(c.Request.Context(), pass) // no timing difference for unknown users
	}
	if !ok {
		if secOn {
			var uu *store.User
			if err == nil {
				uu = u
			}
			s.adminsecLoginFailed(c, login, uu, "Wrong username or password.", attemptID)
		} else {
			s.auth.AttemptFailed(c, attemptID)
		}
		if msg, ok := s.adminsecGenericError(); ok { // same single answer as /wp-login: no account enumeration
			fail(string(msg))
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			if strings.Contains(login, "@") {
				fail("<strong>Error:</strong> Unknown email address. Check again or try your username.")
			} else {
				fail("<strong>Error:</strong> The username <strong>" + gooEsc(login) + "</strong> is not registered on this site. If you are unsure of your username, try your email address instead.")
			}
			return
		}
		fail(`<strong>Error:</strong> The password you entered for the username <strong>` + gooEsc(login) + `</strong> is incorrect. <a href="` + gooEsc(s.gooAccountURL(ctx, "lost-password")) + `">Lost your password?</a>`)
		return
	}
	target := s.gooSafeURL(ctx, c.PostForm("redirect"), account)
	if secOn { // AdminSecGo: password policy, second factor, alerts (it answers the request)
		s.auth.DropAttempt(c, attemptID)
		s.gooSaveSession(c)
		s.adminsecPasswordOK(c, loginView{Title: "Log In", Action: "login", SiteName: s.settings.String("blogname"), HomeURL: s.links(ctx).HomeURL("/"),
			RedirectTo: target, Login: login, Remember: remember}, u, pass)
		return
	}
	s.auth.AttemptSucceeded(c, attemptID, ip, attemptKey)
	if err := s.loginUserCookie(c, u.ID, remember); err != nil {
		fail("<strong>Error:</strong> Could not start a session. Please try again.")
		return
	}
	s.gooRedirect(c, target)
}

// gooLostPasswordHTML is the lost-password endpoint (form-lost-password.php, lost-password-confirmation.php
// and form-reset-password.php): the request form, the "email sent" answer, or the new-password form when
// the URL carries a valid reset key.
func (s *Server) gooLostPasswordHTML(c *gin.Context) string {
	ctx := c.Request.Context()
	action := s.gooAccountURL(ctx, "lost-password")
	notices := s.gooNoticesHTML(c)
	if c.Query("reset-link-sent") == "true" {
		return notices + s.gooTpl("goo/lost-password-confirmation", nil)
	}
	if key, login := c.Query("key"), c.Query("login"); key != "" || login != "" {
		if _, ok := s.checkResetKey(ctx, login, key); ok {
			return notices + s.gooTpl("goo/form-reset-password", map[string]any{
				"Action": action, "Key": key, "Login": login, "Nonce": s.nonce(c, "reset_password"),
			})
		}
		notices += `<ul class="woocommerce-error" role="alert"><li>` + gooInvalidResetKey + `</li></ul>`
	}
	return notices + s.gooTpl("goo/form-lost-password", map[string]any{
		"Action": action, "Nonce": s.nonce(c, "lost_password"), "Captcha": string(s.loginCaptchaWidget(c)),
	})
}

const gooInvalidResetKey = "This key is invalid or has already been used. Please reset your password again if needed."

// gooAddQuery appends k=v to a URL that may already have a query (plain permalinks).
func gooAddQuery(u, kv string) string {
	if strings.Contains(u, "?") {
		return u + "&" + kv
	}
	return u + "?" + kv
}

// gooLostPasswordPost is WC_Form_Handler::process_lost_password() through the core reset: the same
// throttle, mail and key as wp-login.php?action=lostpassword, with the link opening this endpoint. The
// answer is the same, after the same time, whether or not the account exists.
func (s *Server) gooLostPasswordPost(c *gin.Context) {
	ctx := c.Request.Context()
	lost := s.gooAccountURL(ctx, "lost-password")
	fail := func(msg string) {
		s.gooAddNotice(c, "error", msg)
		s.gooRedirect(c, lost)
	}
	if !s.checkNonceValue(c, c.PostForm("woocommerce-lost-password-nonce"), "lost_password") {
		fail("Your session has expired. Please reload the page and try again.")
		return
	}
	if msg := s.loginCaptchaFailed(c); msg != "" { // Pursae Security
		fail(string(msg))
		return
	}
	login := strings.TrimSpace(c.PostForm("user_login"))
	if login == "" {
		fail("Enter a username or email address.")
		return
	}
	start := time.Now()
	// The mailed link is built from the configured home, never this request's Host.
	s.goResetRequestVia(c.Copy(), login, s.gooAccountURL(withConfiguredOrigin(ctx), "lost-password"))
	wait := time.NewTimer(resetResponseTime - time.Since(start))
	select {
	case <-wait.C:
	case <-ctx.Done():
	}
	wait.Stop()
	s.gooRedirect(c, gooAddQuery(lost, "reset-link-sent=true"))
}

// gooResetPasswordPost is WC_Form_Handler::process_reset_password(): the new password for a valid key.
func (s *Server) gooResetPasswordPost(c *gin.Context) {
	ctx := c.Request.Context()
	lost := s.gooAccountURL(ctx, "lost-password")
	key, login := c.PostForm("reset_key"), c.PostForm("reset_login")
	if !s.checkNonceValue(c, c.PostForm("woocommerce-reset-password-nonce"), "reset_password") {
		s.gooAddNotice(c, "error", "Your session has expired. Please reload the page and try again.")
		s.gooRedirect(c, gooAddQuery(lost, "key="+url.QueryEscape(key)+"&login="+url.QueryEscape(login)))
		return
	}
	u, ok := s.checkResetKey(ctx, login, key)
	if !ok {
		s.gooAddNotice(c, "error", gooInvalidResetKey)
		s.gooRedirect(c, lost)
		return
	}
	msg, invalid := s.applyPasswordReset(c, u, key, c.PostForm("password_1"), c.PostForm("password_2"))
	switch {
	case invalid:
		s.gooAddNotice(c, "error", gooInvalidResetKey)
		s.gooRedirect(c, lost)
	case msg != "":
		s.gooAddNotice(c, "error", msg)
		s.gooRedirect(c, gooAddQuery(lost, "key="+url.QueryEscape(key)+"&login="+url.QueryEscape(login)))
	default:
		s.gooAddNotice(c, "success", "Your password has been reset successfully.")
		s.gooRedirect(c, gooAddQuery(s.gooPageURL(ctx, "myaccount"), "password-reset=true"))
	}
}

// gooRegisterPost is WC_Form_Handler::process_registration().
func (s *Server) gooRegisterPost(c *gin.Context) {
	ctx := c.Request.Context()
	account := s.gooPageURL(ctx, "myaccount")
	fail := func(msg string) {
		s.gooAddNotice(c, "error", msg)
		s.gooRedirect(c, account)
	}
	if !s.gooOptBool("enable_myaccount_registration") || !s.checkNonceValue(c, c.PostForm("woocommerce-register-nonce"), "woocommerce-register") {
		fail("<strong>Error:</strong> Registration is not allowed.")
		return
	}
	if msg := s.loginCaptchaFailed(c); msg != "" { // Pursae Security
		fail(string(msg))
		return
	}
	email := strings.TrimSpace(c.PostForm("email"))
	username := ""
	if !s.gooOptBool("registration_generate_username") {
		username = strings.TrimSpace(c.PostForm("username"))
		if username == "" {
			fail("<strong>Error:</strong> Please enter a valid account username.")
			return
		}
	}
	password := ""
	if !s.gooOptBool("registration_generate_password") {
		password = c.PostForm("password")
		if password == "" {
			fail("<strong>Error:</strong> Please enter an account password.")
			return
		}
	}
	id, err := s.gooCreateCustomer(ctx, email, username, password, map[string]string{})
	if err != nil {
		fail("<strong>Error:</strong> " + err.Error())
		return
	}
	if err := s.loginUserCookie(c, id, false); err != nil {
		fail("<strong>Error:</strong> " + gooEsc(err.Error()))
		return
	}
	s.gooRedirect(c, s.gooSafeURL(ctx, c.PostForm("redirect"), account))
}

// gooSaveAddressPost is WC_Form_Handler::save_address().
func (s *Server) gooSaveAddressPost(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	typ := c.PostForm("address_type")
	if typ != "billing" && typ != "shipping" {
		typ = "billing"
	}
	addrURL := s.gooAccountURL(ctx, "edit-address")
	if u == nil || !s.checkNonceValue(c, c.PostForm("woocommerce-edit-address-nonce"), "woocommerce-edit_address") {
		s.gooRedirect(c, s.gooPageURL(ctx, "myaccount"))
		return
	}
	var errs []string
	vals := map[string]string{}
	country := strings.TrimSpace(c.PostForm(typ + "_country"))
	for _, f := range s.gooAddressFields(ctx, typ) {
		v := strings.TrimSpace(c.PostForm(f.Key))
		req := f.Required
		if strings.HasSuffix(f.Key, "_state") {
			req = gooStateRequired(country)
		}
		if strings.HasSuffix(f.Key, "_postcode") && gooPostcodeOptional[country] {
			req = false
		}
		label := s.gooL10nText(c.Request.Context(), f.Label) // inside a sentence: translated on its own
		if req && v == "" {
			errs = append(errs, "<strong>"+gooEsc(label)+"</strong> is a required field.")
			continue
		}
		switch {
		case v == "":
		case f.Type == "email":
			if _, err := mail.ParseAddress(v); err != nil {
				errs = append(errs, "<strong>"+gooEsc(label)+"</strong> is not a valid email address.")
			}
		case f.Type == "tel":
			if !gooPhoneRe.MatchString(v) {
				errs = append(errs, "<strong>"+gooEsc(label)+"</strong> is not a valid phone number.")
			}
		case strings.HasSuffix(f.Key, "_postcode"):
			v = strings.ToUpper(v)
		case f.Type == "state":
			if states, ok := gooStates[country]; ok {
				found := false
				for _, st := range states {
					if strings.EqualFold(st.Code, v) || strings.EqualFold(st.Name, v) {
						v, found = st.Code, true
					}
				}
				if !found {
					errs = append(errs, "<strong>"+gooEsc(label)+"</strong> is not valid. Please enter one of the following: "+gooEsc(gooStateNames(country)))
				}
			}
		}
		vals[f.Key] = v
	}
	if len(errs) > 0 {
		for _, e := range errs {
			s.gooAddNotice(c, "error", e)
		}
		s.gooRedirect(c, s.gooEndpointURL(ctx, "myaccount", s.gooEndpointSlug("edit-address"), typ))
		return
	}
	meta := map[string]any{}
	sess := s.gooSessionFor(c)
	target := sess.Data.Customer.Billing
	if typ == "shipping" {
		target = sess.Data.Customer.Shipping
	}
	for k, v := range vals {
		meta[k] = v
		target[strings.TrimPrefix(k, typ+"_")] = v
	}
	sess.touch()
	_ = s.gooSetUserMeta(ctx, u.ID, meta)
	s.gooAddNotice(c, "success", "Address changed successfully.")
	s.gooRedirect(c, addrURL)
}

// gooSaveAccountPost is WC_Form_Handler::save_account_details().
func (s *Server) gooSaveAccountPost(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	editURL := s.gooAccountURL(ctx, "edit-account")
	if u == nil || !s.checkNonceValue(c, c.PostForm("save-account-details-nonce"), "save_account_details") {
		s.gooRedirect(c, s.gooPageURL(ctx, "myaccount"))
		return
	}
	first, last := strings.TrimSpace(c.PostForm("account_first_name")), strings.TrimSpace(c.PostForm("account_last_name"))
	display, email := strings.TrimSpace(c.PostForm("account_display_name")), strings.TrimSpace(c.PostForm("account_email"))
	cur, p1, p2 := c.PostForm("password_current"), c.PostForm("password_1"), c.PostForm("password_2")
	var errs []string
	for _, f := range [][2]string{{first, "First name"}, {last, "Last name"}, {display, "Display name"}, {email, "Email address"}} {
		if f[0] == "" {
			errs = append(errs, "<strong>"+gooEsc(s.gooL10nText(c.Request.Context(), f[1]))+"</strong> is a required field.")
		}
	}
	if strings.Contains(display, "@") && display != u.Email && display != email {
		errs = append(errs, "Display name cannot be changed to email address due to privacy concern.")
	}
	if email != "" {
		if _, err := mail.ParseAddress(email); err != nil {
			errs = append(errs, "Please provide a valid email address.")
		} else if other, err := s.store.UserByLoginOrEmail(ctx, email); err == nil && other.ID != u.ID {
			errs = append(errs, "This email address is already registered.")
		}
	}
	// A new e-mail address needs the current password too (it is how the account is recovered) and is
	// applied only once confirmed from the new inbox, as on the wp-admin profile.
	emailChange := email != "" && !strings.EqualFold(email, u.Email)
	changePass := p1 != "" || p2 != "" || (cur != "" && !emailChange)
	if changePass {
		switch {
		case cur == "":
			errs = append(errs, "Please enter your current password.")
		case p1 == "":
			errs = append(errs, "Please fill out all password fields.")
		case p2 == "":
			errs = append(errs, "Please re-enter your password.")
		case p1 != p2:
			errs = append(errs, "New passwords do not match.")
		}
	} else if emailChange && cur == "" {
		errs = append(errs, "Please enter your current password to change your email address.")
	}
	if len(errs) == 0 && (changePass || emailChange) {
		// The same per-account budget as the login form: the current password is a guessable secret.
		id, key, throttled := s.coreBeginAttempt(c, u.Login)
		switch {
		case throttled:
			errs = append(errs, "Too many failed attempts. Please try again in 15 minutes.")
		case !s.checkUserPassword(ctx, u, cur):
			s.auth.AttemptFailed(c, id)
			errs = append(errs, "Your current password is incorrect.")
		default:
			s.auth.AttemptSucceeded(c, id, clientIP(c), key)
			if changePass {
				if msg := s.adminsecPasswordProblem(u.Roles, u.Login, u.Email, p1); msg != "" { // Pursae Security password policy
					errs = append(errs, msg)
				}
			}
		}
	}
	if len(errs) > 0 {
		for _, e := range errs {
			s.gooAddNotice(c, "error", e)
		}
		s.gooRedirect(c, editURL)
		return
	}
	if u.Profile == nil {
		u.Profile = map[string]string{}
	}
	u.Profile["first_name"], u.Profile["last_name"] = first, last
	u.DisplayName = display
	if !emailChange {
		u.Email = email // a change of letter case only
	}
	if err := s.store.UpdateUserProfile(ctx, u); err != nil {
		s.gooAddNotice(c, "error", "Could not save your account details.")
		s.gooRedirect(c, editURL)
		return
	}
	s.forumPurge(ctx) // Pursae Forum: display names show on cached forum pages (a no-op tag when the forum is off)
	if changePass {
		if hash, err := auth.HashPassword(p1); err == nil {
			if s.store.SetUserPassword(ctx, u.ID, hash) == nil {
				s.adminsecPasswordChanged(c, u)
				s.endSessionsAfterPasswordChange(ctx, c, u.ID)
			}
		}
	}
	if emailChange {
		s.sendEmailChangeConfirmation(c, u, email)
		s.gooAddNotice(c, "notice", "Your email address has not been updated yet. Please check your inbox at "+gooEsc(email)+" for a confirmation email.")
	}
	s.gooAddNotice(c, "success", "Account details changed successfully.")
	s.gooRedirect(c, s.gooPageURL(ctx, "myaccount"))
}

// gooLogout is the customer-logout endpoint (nonce-protected; asks for confirmation without one).
func (s *Server) gooLogout(c *gin.Context) bool {
	ctx := c.Request.Context()
	account := s.gooPageURL(ctx, "myaccount")
	if currentUser(c) == nil {
		s.gooRedirect(c, account)
		return true
	}
	if !s.checkNonce(c, "customer-logout") {
		s.gooAddNotice(c, "notice", `Are you sure you want to log out? <a href="`+gooEsc(s.gooLogoutURL(c))+`">Confirm and log out</a>`)
		s.gooRedirect(c, account)
		return true
	}
	if sess := currentSession(c); sess != nil {
		s.adminsecLogout(c)
		_ = s.auth.Destroy(c, sess.Token)
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	for _, ck := range []string{gooItemsCookie, gooCartHashCook} {
		gooClearCookie(c, ck)
	}
	gooNoStore(c)
	c.Redirect(http.StatusFound, account)
	return true
}

// Downloads ------------------------------------------------------------------------------------------------------

type gooFrontDownloadRow struct {
	ProductID   int64
	ProductName string
	ProductURL  string
	DownloadID  string
	FileName    string
	URL         string
	Remaining   *int
	Expires     *time.Time
	OrderID     int64
}

// gooDownloadRows reads permission rows (with the order paid, not exhausted, not expired).
func (s *Server) gooDownloadRows(c *gin.Context, where string, args ...any) []gooFrontDownloadRow {
	ctx := c.Request.Context()
	statuses := "'completed'"
	if s.gooDownloadPermitted("processing") {
		statuses = "'processing','completed'"
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT dp.product_id, dp.download_id, dp.order_id, dp.order_key, dp.user_email, dp.downloads_remaining, dp.access_expires
		FROM goo_download_permissions dp JOIN goo_orders o ON o.id = dp.order_id
		WHERE o.status IN (`+statuses+`) AND (dp.downloads_remaining IS NULL OR dp.downloads_remaining > 0)
		AND (dp.access_expires IS NULL OR dp.access_expires > now()) AND `+where+` ORDER BY dp.order_id DESC, dp.id`, args...)
	if err != nil {
		return nil
	}
	type raw struct {
		pid       int64
		did       string
		oid       int64
		key       string
		email     string
		remaining *int
		expires   *time.Time
	}
	var list []raw
	for rows.Next() {
		var r raw
		if rows.Scan(&r.pid, &r.did, &r.oid, &r.key, &r.email, &r.remaining, &r.expires) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	var out []gooFrontDownloadRow
	lb := s.links(ctx)
	for _, r := range list {
		p, err := s.gooGetProduct(ctx, r.pid)
		if err != nil {
			continue
		}
		var file *gooDownload
		for i := range p.Downloads {
			if p.Downloads[i].ID == r.did {
				file = &p.Downloads[i]
			}
		}
		if file == nil {
			continue
		}
		u := lb.HomeURL("/?download_file=" + itoa64(r.pid) + "&order=" + urlEscape(r.key) + "&email=" + urlEscape(r.email) + "&key=" + urlEscape(r.did))
		out = append(out, gooFrontDownloadRow{ProductID: r.pid, ProductName: s.gooProductTitle(ctx, p), ProductURL: s.gooPermalink(ctx, p), DownloadID: r.did,
			FileName: defaultStr(file.Name, filepath.Base(file.File)), URL: u, Remaining: r.remaining, Expires: r.expires, OrderID: r.oid})
	}
	return out
}

// gooCustomerDownloads is wc_get_customer_available_downloads(): the account's own permissions only.
// Guest purchases are not matched by e-mail, since the account's address is not proven to be the
// buyer's (registration and Edit Account take any address).
func (s *Server) gooCustomerDownloads(c *gin.Context, u *store.User) []gooFrontDownloadRow {
	return s.gooDownloadRows(c, "dp.user_id = $1", u.ID)
}

// gooOrderDownloads are an order's downloads (order received / view order).
func (s *Server) gooOrderDownloads(c *gin.Context, o *gooOrder) []gooFrontDownloadRow {
	return s.gooDownloadRows(c, "dp.order_id = $1", o.ID)
}

// gooDownloadsTable is order-downloads.php.
func (s *Server) gooDownloadsTable(c *gin.Context, rows []gooFrontDownloadRow) string {
	if len(rows) == 0 {
		return ""
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].OrderID > rows[j].OrderID })
	var b strings.Builder
	b.WriteString(`<table class="woocommerce-table woocommerce-table--order-downloads shop_table shop_table_responsive order_details"><thead><tr>` +
		`<th class="download-product"><span class="nobr">Product</span></th><th class="download-remaining"><span class="nobr">Downloads remaining</span></th>` +
		`<th class="download-expires"><span class="nobr">Expires</span></th><th class="download-file"><span class="nobr">Download</span></th></tr></thead>`)
	for _, r := range rows {
		rem := "&infin;"
		if r.Remaining != nil {
			rem = strconv.Itoa(*r.Remaining)
		}
		exp := "Never"
		if r.Expires != nil {
			t := r.Expires.In(s.loc())
			exp = `<time datetime="` + t.Format("2006-01-02") + `" title="` + strconv.FormatInt(t.Unix(), 10) + `">` + wp.FormatDate(t, s.settings.String("date_format")) + `</time>`
		}
		b.WriteString(`<tr><td class="download-product" data-title="Product"><a href="` + gooEsc(r.ProductURL) + `">` + gooEsc(r.ProductName) + `</a></td>` +
			`<td class="download-remaining" data-title="Downloads remaining">` + rem + `</td><td class="download-expires" data-title="Expires">` + exp + `</td>` +
			`<td class="download-file" data-title="Download"><a href="` + gooEsc(r.URL) + `" class="woocommerce-MyAccount-downloads-file button alt">` + gooEsc(r.FileName) + `</a></td></tr>`)
	}
	b.WriteString(`</table>`)
	return b.String()
}

// gooDownload is WC_Download_Handler::download_product(): permission, limits, expiry, login, then
// the file by the configured method.
func (s *Server) gooDownload(c *gin.Context) {
	ctx := c.Request.Context()
	q := c.Request.URL.Query()
	pid := gooAtoi64(q.Get("download_file"))
	orderKey, email, key := q.Get("order"), q.Get("email"), q.Get("key")
	die := func(code int, msg string) {
		gooNoStore(c)
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(code, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Download error</title><link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head>`+
			`<body id="error-page"><div class="wp-die-message"><p>`+msg+`</p><p><a href="`+gooEsc(s.gooPageURL(ctx, "shop"))+`" class="wc-forward">Go to shop</a></p></div></body></html>`)
	}
	if !s.gooOrderGetAllowed(c, orderKey) {
		die(http.StatusTooManyRequests, gooThrottledMsg)
		return
	}
	var id, oid, uid int64
	var permEmail string
	var remaining *int
	var expires *time.Time
	err := s.store.Pool.QueryRow(ctx, `SELECT id, order_id, user_id, user_email, downloads_remaining, access_expires FROM goo_download_permissions
		WHERE product_id = $1 AND order_key = $2 AND download_id = $3 ORDER BY id LIMIT 1`, pid, orderKey, key).Scan(&id, &oid, &uid, &permEmail, &remaining, &expires)
	// A guest link always has to name the billing e-mail. downloads_require_login replaces that
	// with a login, but a supplied e-mail that does not match is still refused.
	emailOK := strings.EqualFold(strings.TrimSpace(email), permEmail)
	if errors.Is(err, pgx.ErrNoRows) || err != nil || (email != "" && !emailOK) || (!s.gooOptBool("downloads_require_login") && !emailOK) {
		s.gooOrderGetFailed(c, orderKey)
		die(http.StatusForbidden, "Invalid download link.")
		return
	}
	o, err := s.gooGetOrder(ctx, oid)
	if err != nil || !s.gooDownloadPermitted(o.Status) {
		die(http.StatusForbidden, "Invalid order.")
		return
	}
	u := currentUser(c)
	if s.gooOptBool("downloads_require_login") {
		if u == nil {
			s.gooAddNotice(c, "error", "You must be logged in to download files.")
			s.gooRedirect(c, s.gooPageURL(ctx, "myaccount"))
			return
		}
		if (uid != 0 && uid != u.ID) || (uid == 0 && !strings.EqualFold(u.Email, permEmail)) {
			die(http.StatusForbidden, "This is not a valid download link.")
			return
		}
	}
	if remaining != nil && *remaining <= 0 {
		die(http.StatusForbidden, "Sorry, you have reached your download limit for this file")
		return
	}
	if expires != nil && time.Now().After(*expires) {
		die(http.StatusForbidden, "Sorry, this download has expired")
		return
	}
	p, err := s.gooGetProduct(ctx, pid)
	var file *gooDownload
	if err == nil {
		for i := range p.Downloads {
			if p.Downloads[i].ID == key {
				file = &p.Downloads[i]
			}
		}
	}
	if file == nil || file.File == "" {
		die(http.StatusNotFound, "No file defined")
		return
	}
	// The decrement is the gate (a claim): parallel requests cannot all pass the limit check above.
	var claimed int64
	if err := s.store.Pool.QueryRow(ctx, `UPDATE goo_download_permissions SET download_count = download_count + 1,
		downloads_remaining = downloads_remaining - 1
		WHERE id = $1 AND (downloads_remaining IS NULL OR downloads_remaining > 0) AND (access_expires IS NULL OR access_expires > now())
		RETURNING id`, id).Scan(&claimed); errors.Is(err, pgx.ErrNoRows) {
		die(http.StatusForbidden, "Sorry, you have reached your download limit for this file")
		return
	} else if err != nil {
		die(http.StatusInternalServerError, "The download could not be started. Please try again.")
		return
	}
	gooNoStore(c)
	// A private file (Media Library › Make private) has no public URL: it is sent by this handler, or
	// through a link that expires in minutes (a signed site URL, a presigned URL of the bucket).
	if rel := s.mediaPrivateRelOfURL(ctx, file.File); rel != "" {
		if !s.mediaPrivateDeliver(c, rel, true, s.gooOptString("file_download_method") == "redirect") {
			die(http.StatusNotFound, "No file defined")
		}
		return
	}
	if s.gooOptString("file_download_method") != "redirect" {
		if local := s.gooLocalFile(ctx, file.File); local != "" {
			name := filepath.Base(local)
			c.Header("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
			c.Header("X-Robots-Tag", "noindex, nofollow")
			c.File(local)
			return
		}
	}
	c.Redirect(http.StatusFound, file.File)
}

// gooOrderGetAllowed is the sliding window for order-key lookups (downloads and GET /order/{id}).
// Failures are counted per IP and per key so one visitor cannot guess many keys and one key
// cannot be hammered from many addresses. Successful lookups are not counted.
func (s *Server) gooOrderGetAllowed(c *gin.Context, key string) bool {
	if gooOrderGetThrottle.Blocked(clientIP(c)) {
		return false
	}
	return key == "" || !gooOrderGetThrottle.Blocked("order:"+key)
}

func (s *Server) gooOrderGetFailed(c *gin.Context, key string) {
	gooOrderGetThrottle.Fail(clientIP(c))
	if key != "" {
		gooOrderGetThrottle.Fail("order:" + key)
	}
}

// gooLocalFile maps a file URL in the uploads directory to its path ("" when remote or missing).
func (s *Server) gooLocalFile(ctx context.Context, file string) string {
	if s.cfg.UploadsDir == "" {
		return ""
	}
	rel := ""
	home := strings.TrimRight(s.links(ctx).HomeURL("/"), "/")
	for _, prefix := range []string{home + "/wp-content/uploads/", "/wp-content/uploads/"} {
		if strings.HasPrefix(file, prefix) {
			rel = strings.TrimPrefix(file, prefix)
			break
		}
	}
	if rel == "" || strings.Contains(rel, "..") {
		return ""
	}
	p := filepath.Join(s.cfg.UploadsDir, filepath.FromSlash(rel))
	if st, err := os.Stat(s.gocdnFetchBack(ctx, p)); err != nil || st.IsDir() {
		return ""
	}
	return p
}

// Order tracking -------------------------------------------------------------------------------------------------

// gooOrderTrackingHTML is the [goocommerce_order_tracking] shortcode.
func (s *Server) gooOrderTrackingHTML(c *gin.Context) string {
	ctx := c.Request.Context()
	var b strings.Builder
	b.WriteString(`<div class="woocommerce">`)
	b.WriteString(s.gooNoticesHTML(c))
	oid := strings.TrimPrefix(strings.TrimSpace(c.PostForm("orderid")), "#")
	email := strings.TrimSpace(c.PostForm("order_email"))
	if c.Request.Method == http.MethodPost && c.PostForm("woocommerce-order-tracking-nonce") != "" {
		switch {
		case !s.checkNonceValue(c, c.PostForm("woocommerce-order-tracking-nonce"), "woocommerce-order_tracking"):
			b.WriteString(gooErrorList([]string{"Your session has expired. Please reload the page and try again."}))
		case oid == "":
			b.WriteString(gooErrorList([]string{"Please enter a valid order ID"}))
		case email == "":
			b.WriteString(gooErrorList([]string{"Please enter a valid email address"}))
		case gooTrackingThrottle.Blocked(clientIP(c)):
			b.WriteString(gooErrorList([]string{gooThrottledMsg}))
		default:
			o, err := s.gooGetOrder(ctx, gooAtoi64(oid))
			if err == nil && o.Type == "shop_order" && strings.EqualFold(o.Billing["email"], email) {
				b.WriteString(`<p class="order-info">` + strings.TrimSuffix(strings.TrimPrefix(s.gooOrderStatusText(o), "<p>"), "</p>") + `</p>`)
				b.WriteString(s.gooOrderNotesHTML(c, o))
				b.WriteString(s.gooOrderDetailsHTML(c, o, false))
				b.WriteString(`</div>`)
				return b.String()
			}
			gooTrackingThrottle.Fail(clientIP(c))
			b.WriteString(gooErrorList([]string{"Sorry, the order could not be found. Please contact us if you are having difficulty finding your order details."}))
		}
	}
	b.WriteString(s.gooTpl("goo/form-tracking", map[string]any{"Action": s.gooCurrentURL(c), "OrderID": oid, "Email": email, "Nonce": s.nonce(c, "woocommerce-order_tracking")}))
	b.WriteString(`</div>`)
	return b.String()
}
