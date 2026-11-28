package server

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Gift Cards on the storefront and in the product editor: the gift card form on the product page,
// "Have a gift card?" on the cart and checkout (apply / remove routes), My Account › Gift Cards and
// the product's "Gift card" panel.

func init() {
	gooOnHTML("single_before_add_to_cart", gcProductForm)
	gooOnPage("cart_after", func(s *Server, c *gin.Context) template.HTML {
		if !s.gooExtOn(gcKey) || !s.gooExtBool(gcKey, "enable_cart") {
			return ""
		}
		return s.gcRedeemForm(c, "cart")
	})
	gooOnPage("checkout_before_payment", func(s *Server, c *gin.Context) template.HTML {
		if !s.gooExtOn(gcKey) || !s.gooExtBool(gcKey, "enable_checkout") {
			return ""
		}
		return s.gcRedeemForm(c, "checkout")
	})
	gooFrontRoutes = append(gooFrontRoutes, gcFrontRoute)
	gooAccountEndpoints = append(gooAccountEndpoints, &gooAccountEndpoint{Slug: "giftcards", Title: "Gift Cards", Module: gcKey, After: "downloads", Render: gcAccount})
	gooProductPanels = append(gooProductPanels, &gooProductPanel{ID: "gift_card_options", Label: "Gift card", Module: gcKey, ShowFor: []string{gcType}, Render: gcPanel, Save: gcPanelSave})
}

// gcPriceHTML is the amounts range ("$25.00 – $100.00") or "From: $X" with a custom amount.
func gcPriceHTML(s *Server, ctx context.Context, p *gooProduct, h template.HTML) template.HTML {
	if !s.gooExtOn(gcKey) || p.Type != gcType {
		return h
	}
	amounts := gcAmounts(p)
	custom := gxBool(p.Meta["_gift_card_custom_amount"])
	lo, hi := 0.0, 0.0
	if len(amounts) > 0 {
		lo, hi = amounts[0], amounts[len(amounts)-1]
	}
	if custom {
		if mn := gxNum(p.Meta["_gift_card_min"]); mn > 0 && (lo == 0 || mn < lo) {
			lo = mn
		}
		if mx := gxNum(p.Meta["_gift_card_max"]); mx > hi {
			hi = mx
		} else if mx <= 0 {
			return template.HTML(`<span class="from">From: </span>`) + s.gooPrice(lo)
		}
	}
	if lo == hi {
		return s.gooPrice(lo)
	}
	return s.gooPrice(lo) + ` <span aria-hidden="true">&ndash;</span> ` + s.gooPrice(hi)
}

// gcProductForm is the gift card form (amount, To, From, Message, Delivery date).
func gcProductForm(s *Server, c *gin.Context, p *gooProduct) template.HTML {
	if !s.gooExtOn(gcKey) || p.Type != gcType {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="woocommerce_gc_giftcard_form">`)
	amounts := gcAmounts(p)
	custom := gxBool(p.Meta["_gift_card_custom_amount"])
	if len(amounts) > 0 {
		b.WriteString(`<div class="wc_gc_field wc_gc_giftcard_amount"><label for="wc_gc_giftcard_amount">Amount</label><select id="wc_gc_giftcard_amount" name="wc_gc_giftcard_amount" required>`)
		for _, a := range amounts {
			fmt.Fprintf(&b, `<option value="%s">%s</option>`, gxFmt(a), html.EscapeString(s.gooPricePlain(a, s.gooCurrency())))
		}
		if custom {
			b.WriteString(`<option value="custom">Other amount</option>`)
		}
		b.WriteString(`</select></div>`)
	}
	if custom {
		mn, mx := gxNum(p.Meta["_gift_card_min"]), gxNum(p.Meta["_gift_card_max"])
		attrs := ` min="` + gxFmt(max(mn, 0.01)) + `"`
		if mx > 0 {
			attrs += ` max="` + gxFmt(mx) + `"`
		}
		fmt.Fprintf(&b, `<div class="wc_gc_field wc_gc_giftcard_custom_amount"><label for="wc_gc_giftcard_custom_amount">%s</label><input type="number" step="any" id="wc_gc_giftcard_custom_amount" name="wc_gc_giftcard_custom_amount"%s class="input-text"></div>`,
			map[bool]string{true: "Other amount", false: "Amount"}[len(amounts) > 0], attrs)
	}
	field := func(id, label, input string) {
		fmt.Fprintf(&b, `<div class="wc_gc_field %s"><label for="%s">%s</label>%s</div>`, id, id, label, input)
	}
	if ds := gcProductDesigns(p); len(ds) > 1 {
		var o strings.Builder
		for _, k := range ds {
			fmt.Fprintf(&o, `<option value="%s">%s</option>`, k, html.EscapeString(gcDesignFor(k).Label))
		}
		field("wc_gc_giftcard_design", "Design", `<select id="wc_gc_giftcard_design" name="wc_gc_giftcard_design">`+o.String()+`</select>`)
	}
	field("wc_gc_giftcard_to", "To", `<input type="email" id="wc_gc_giftcard_to" name="wc_gc_giftcard_to" placeholder="Enter recipient e-mail" class="input-text" required>`)
	field("wc_gc_giftcard_to_name", "Recipient name", `<input type="text" id="wc_gc_giftcard_to_name" name="wc_gc_giftcard_to_name" class="input-text">`)
	from := ""
	if u := currentUser(c); u != nil {
		from = u.DisplayName
	}
	field("wc_gc_giftcard_from", "From", `<input type="text" id="wc_gc_giftcard_from" name="wc_gc_giftcard_from" placeholder="Enter your name" value="`+html.EscapeString(from)+`" class="input-text" required>`)
	field("wc_gc_giftcard_message", "Message", `<textarea id="wc_gc_giftcard_message" name="wc_gc_giftcard_message" rows="3" maxlength="500" placeholder="Add your message (optional)" class="input-text"></textarea>`)
	field("wc_gc_giftcard_delivery", "Delivery date", `<input type="date" id="wc_gc_giftcard_delivery" name="wc_gc_giftcard_delivery" min="`+time.Now().In(s.gxLoc()).Format("2006-01-02")+`" class="input-text"> <small>Leave empty to send the gift card now.</small>`)
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// gcRedeemForm is "Have a gift card?" with the applied cards and their Remove links.
func (s *Server) gcRedeemForm(c *gin.Context, where string) template.HTML {
	ctx := c.Request.Context()
	cart := s.gooCartFor(c)
	home := s.links(c).HomeURL("/")
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="wc_gc_giftcard_form woocommerce-form-gift-card add_gift_card_form" data-where="%s">`, where)
	if fees := gcCartGiftCards(cart); len(fees) > 0 {
		b.WriteString(`<table class="shop_table wc_gc_applied_giftcards"><tbody>`)
		nonce := s.nonce(c, "wc_gc_remove_giftcard")
		for _, f := range fees {
			code := gxStr(f.Meta["gift_card_code"])
			bal := ""
			if g, err := s.gcByCode(ctx, code); err == nil {
				bal = fmt.Sprintf(` <small class="wc_gc_balance">(Remaining balance: %s)</small>`, html.EscapeString(s.gooPricePlain(g.Balance+f.Amount, g.Currency)))
			}
			fmt.Fprintf(&b, `<tr class="wc_gc_giftcard"><th>Gift card <code>%s</code>%s</th><td>%s <a href="%s" class="wc_gc_remove_gift_card">[Remove]</a></td></tr>`,
				html.EscapeString(code), bal, s.gooPrice(f.Amount), html.EscapeString(home+"?goo-ajax=remove_gift_card&giftcard="+url.QueryEscape(code)+"&redirect="+where+"&_wpnonce="+nonce))
		}
		b.WriteString(`</tbody></table>`)
	}
	input := `<input type="text" name="wc_gc_cart_code" class="input-text" id="wc_gc_cart_code_` + where + `" placeholder="Enter your code&hellip;" autocomplete="off">`
	button := `<button type="submit" class="button wc_gc_button" name="wc_gc_cart_redeem_send">Apply</button>`
	if where == "cart" {
		fmt.Fprintf(&b, `<form method="post" action="%s"><input type="hidden" name="goo_module" value="gift_cards"><input type="hidden" name="redirect" value="cart">`+
			`<input type="hidden" name="_wpnonce" value="%s"><h4 class="wc_gc_add_gift_card_form__title">Have a gift card?</h4><p class="form-row">%s %s</p></form>`,
			html.EscapeString(home+"?goo-ajax=apply_gift_card"), s.nonce(c, "wc_gc_apply_giftcard"), input, button)
	} else {
		// Inside the checkout form: no nested form, the code is sent by fetch.
		fmt.Fprintf(&b, `<h4 class="wc_gc_add_gift_card_form__title">Have a gift card?</h4><p class="form-row" data-action="%s" data-nonce="%s">%s <button type="button" class="button wc_gc_button">Apply</button></p>`+
			`<script`+cspNonceAttr()+`>(function(){var p=document.currentScript.previousElementSibling;p.querySelector('button').addEventListener('click',function(){var d=new FormData();d.append('goo_module','gift_cards');`+
			`d.append('wc_gc_cart_code',p.querySelector('input').value);d.append('_wpnonce',p.dataset.nonce);fetch(p.dataset.action,{method:'POST',body:d,credentials:'same-origin',headers:{'X-Requested-With':'XMLHttpRequest'}})`+
			`.then(function(r){return r.json()}).then(function(r){if(r.result==='success'){location.reload()}else{alert(r.message)}})})})();</script>`,
			html.EscapeString(home+"?goo-ajax=apply_gift_card"), s.nonce(c, "wc_gc_apply_giftcard"), input)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// gcFrontRoute serves apply_gift_card, remove_gift_card and add_gift_card_to_account.
func gcFrontRoute(s *Server, c *gin.Context, path string) bool {
	if !s.gooExtOn(gcKey) {
		return false
	}
	switch c.Query("goo-ajax") {
	case "print_gift_card":
		s.gcPrintPage(c)
		return true
	case "apply_gift_card":
		if c.Request.Method != http.MethodPost {
			return false
		}
		msg, err := s.gcApply(c, c.PostForm("wc_gc_cart_code"))
		s.gcRespond(c, msg, err)
		return true
	case "remove_gift_card":
		if !s.checkNonceValue(c, c.Query("_wpnonce"), "wc_gc_remove_giftcard") {
			s.gcRespond(c, "", fmt.Errorf("The link you followed has expired."))
			return true
		}
		code := gcNormalize(c.Query("giftcard"))
		sess := s.gooSessionFor(c)
		var keep []string
		for _, x := range gcApplied(sess) {
			if x != code {
				keep = append(keep, x)
			}
		}
		gcSetApplied(sess, keep)
		s.gcRespond(c, "Gift card removed.", nil)
		return true
	case "add_gift_card_to_account":
		if c.Request.Method != http.MethodPost {
			return false
		}
		msg, err := s.gcAddToAccount(c, c.PostForm("wc_gc_redeem_code"))
		s.gcRespond(c, msg, err)
		return true
	}
	return false
}

// gcApply adds a code to the session after checking it.
func (s *Server) gcApply(c *gin.Context, code string) (string, error) {
	ctx := c.Request.Context()
	if c.Request.Method == http.MethodPost && c.PostForm("_wpnonce") != "" && !s.checkNonceValue(c, c.PostForm("_wpnonce"), "wc_gc_apply_giftcard") {
		return "", fmt.Errorf("The link you followed has expired.")
	}
	code = gcNormalize(code)
	if code == "" {
		return "", fmt.Errorf("Please enter a gift card code.")
	}
	// A per-IP limit on codes that do not match keeps codes (and their balances) from being guessed.
	th := s.gooGiftCardThrottle()
	if th.Blocked(clientIP(c)) {
		return "", errGooThrottled
	}
	g, err := s.gcByCode(ctx, code)
	if err != nil {
		th.Fail(clientIP(c))
		return "", fmt.Errorf("Gift card not found. Please enter a valid code.")
	}
	if err := s.gcUsable(g); err != nil {
		return "", err
	}
	sess := s.gooSessionFor(c)
	have := gcApplied(sess)
	if containsStr(have, g.Code) {
		return "", fmt.Errorf("This gift card has already been applied.")
	}
	gcSetApplied(sess, append(have, g.Code))
	if v, ok := c.Get("goo.cart"); ok {
		if cart, ok := v.(*gooCart); ok {
			cart.Calculate()
		}
	}
	return "Gift card code applied successfully.", nil
}

// gcAddToAccount links a card to the customer's account ("Add gift card to your account").
func (s *Server) gcAddToAccount(c *gin.Context, code string) (string, error) {
	ctx := c.Request.Context()
	u := currentUser(c)
	if u == nil {
		return "", fmt.Errorf("Please log in to add a gift card to your account.")
	}
	if !s.checkNonceValue(c, c.PostForm("_wpnonce"), "wc_gc_redeem_giftcard") {
		return "", fmt.Errorf("The link you followed has expired.")
	}
	th := s.gooGiftCardThrottle()
	if th.Blocked(clientIP(c)) {
		return "", errGooThrottled
	}
	g, err := s.gcByCode(ctx, code)
	if err != nil {
		th.Fail(clientIP(c))
		return "", fmt.Errorf("Gift card not found. Please enter a valid code.")
	}
	if g.RedeemedBy != 0 && g.RedeemedBy != u.ID {
		return "", fmt.Errorf("This gift card has already been added to another account.")
	}
	if err := s.gcUsable(g); err != nil {
		return "", err
	}
	if _, err := s.store.Pool.Exec(ctx, `UPDATE goo_gift_cards SET redeemed_by = $2 WHERE id = $1`, g.ID, u.ID); err != nil {
		return "", err
	}
	_ = s.gcLog(ctx, g.ID, "manual", 0, 0, u.ID, "Added to account")
	return "Gift card added to your account.", nil
}

// gcRespond answers JSON to AJAX calls, else adds a notice and goes back to the page.
func (s *Server) gcRespond(c *gin.Context, msg string, err error) {
	ctx := c.Request.Context()
	ajax := c.GetHeader("X-Requested-With") == "XMLHttpRequest" || strings.Contains(c.GetHeader("Accept"), "application/json")
	if err != nil {
		if ajax {
			s.gooSaveSession(c)
			c.JSON(http.StatusOK, gin.H{"result": "failure", "message": err.Error()})
			return
		}
		s.gooAddNotice(c, "error", html.EscapeString(err.Error()))
	} else {
		if ajax {
			s.gooSaveSession(c)
			c.JSON(http.StatusOK, gin.H{"result": "success", "message": msg})
			return
		}
		s.gooAddNotice(c, "success", html.EscapeString(msg))
	}
	s.gooSaveSession(c)
	back := s.gooPageURL(ctx, "cart")
	switch defaultStr(c.PostForm("redirect"), c.Query("redirect")) {
	case "checkout":
		back = s.gooPageURL(ctx, "checkout")
	case "account":
		back = s.gooAccountURL(ctx, "giftcards")
	}
	c.Redirect(http.StatusFound, back)
}

// gcAccount is My Account › Gift Cards: the cards sent to the customer's e-mail or added to the account.
func gcAccount(s *Server, c *gin.Context, arg string) template.HTML {
	if !s.gooExtOn(gcKey) || !s.gooExtBool(gcKey, "account_cards") {
		return ""
	}
	u := currentUser(c)
	if u == nil {
		return ""
	}
	ctx := c.Request.Context()
	cards := s.gcQuery(ctx, `(lower(recipient) = lower($1) OR redeemed_by = $2) AND (delivered OR redeemed_by = $2) ORDER BY created DESC`, u.Email, u.ID)
	var b strings.Builder
	total := 0.0
	// The account's e-mail address is not verified (a customer can change it under Account details, or
	// register with an address that has no account yet), so a card merely addressed to it shows its code
	// masked and no balance: the full code is in the recipient's e-mail, and adding it here (by code)
	// makes it the account's.
	for _, g := range cards {
		if g.RedeemedBy == u.ID && s.gcUsable(g) == nil {
			total += g.Balance
		}
	}
	fmt.Fprintf(&b, `<div class="woocommerce-MyAccount-giftcards"><p class="wc_gc_account_balance">Your gift cards balance: <strong>%s</strong></p>`, s.gooPrice(total))
	if len(cards) == 0 {
		b.WriteString(`<div class="woocommerce-info">You do not have any gift cards yet.</div>`)
	} else {
		b.WriteString(`<table class="shop_table shop_table_responsive my_account_giftcards"><thead><tr><th>Code</th><th>From</th><th>Balance</th><th>Expires</th><th>Status</th></tr></thead><tbody>`)
		for _, g := range cards {
			exp := "&ndash;"
			if g.Expires != nil {
				exp = html.EscapeString(s.gxDate(*g.Expires))
			}
			status := "Active"
			if err := s.gcUsable(g); err != nil {
				status = map[string]string{"disabled": "Disabled", "redeemed": "Used"}[g.Status]
				if status == "" {
					status = "Expired"
				}
			}
			code, balance := g.Code, s.gooPriceIn(g.Balance, g.Currency)
			if g.RedeemedBy != u.ID {
				code, balance, status = gcMaskCode(g.Code), "&ndash;", "Not added"
			}
			fmt.Fprintf(&b, `<tr><td data-title="Code"><code>%s</code></td><td data-title="From">%s</td><td data-title="Balance">%s</td><td data-title="Expires">%s</td><td data-title="Status">%s</td></tr>`,
				html.EscapeString(code), html.EscapeString(g.Sender), balance, exp, status)
		}
		b.WriteString(`</tbody></table>`)
	}
	fmt.Fprintf(&b, `<form method="post" action="%s" class="wc_gc_redeem_form"><h3>Add a gift card to your account</h3><input type="hidden" name="goo_module" value="gift_cards"><input type="hidden" name="redirect" value="account">`+
		`<input type="hidden" name="_wpnonce" value="%s"><p class="form-row"><input type="text" name="wc_gc_redeem_code" class="input-text" placeholder="Enter your code&hellip;"> <button type="submit" class="button">Add</button></p></form></div>`,
		html.EscapeString(s.links(c).HomeURL("/")+"?goo-ajax=add_gift_card_to_account"), s.nonce(c, "wc_gc_redeem_giftcard"))
	return template.HTML(b.String())
}

// Product panel -------------------------------------------------------------------------------------------

func gcPanel(s *Server, c *gin.Context, p *gooProduct) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="options_group gift_card_options">`)
	b.WriteString(bkField("Amounts", "_gift_card_amounts", bkInput("_gift_card_amounts", "text", gxStr(p.Meta["_gift_card_amounts"]), ` placeholder="25, 50, 100"`), "Gift card amounts the customer chooses from, separated by commas."))
	b.WriteString(bkField("Custom amount", "_gift_card_custom_amount", bkCheckbox("_gift_card_custom_amount", gxBool(p.Meta["_gift_card_custom_amount"])), "Let customers enter an amount of their choice."))
	num := func(k string) string {
		if v := gxNum(p.Meta[k]); v > 0 {
			return gxFmt(v)
		}
		return ""
	}
	b.WriteString(bkField("Minimum amount", "_gift_card_min", bkInput("_gift_card_min", "text", num("_gift_card_min"), ` class="wc_input_price"`), ""))
	b.WriteString(bkField("Maximum amount", "_gift_card_max", bkInput("_gift_card_max", "text", num("_gift_card_max"), ` class="wc_input_price"`), "Leave empty for no maximum."))
	b.WriteString(bkField("Designs", "_gift_card_designs", bkInput("_gift_card_designs", "text", gxStr(p.Meta["_gift_card_designs"]), ` placeholder="classic, birthday, holiday"`), "Designs the buyer can choose from, separated by commas: "+strings.Join(gcDesignKeys(), ", ")+". Leave empty for the classic design only."))
	b.WriteString(bkField("Expire in (days)", "_gift_card_expiration_days", bkInput("_gift_card_expiration_days", "number", num("_gift_card_expiration_days"), ` min="0" step="1"`), "Days a gift card can be used after it is issued. Leave empty for gift cards that never expire."))
	b.WriteString(`<p class="form-field"><label>Delivery</label><span class="description">Gift cards are delivered by e-mail; physical delivery is not available.</span></p></div>`)
	return template.HTML(b.String())
}

func gcPanelSave(s *Server, c *gin.Context, p *gooProduct) {
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	var amounts []string
	tmp := &gooProduct{Meta: map[string]any{"_gift_card_amounts": c.PostForm("_gift_card_amounts")}}
	for _, a := range gcAmounts(tmp) {
		amounts = append(amounts, gxFmt(a))
	}
	p.Meta["_gift_card_amounts"] = strings.Join(amounts, ", ")
	p.Meta["_gift_card_custom_amount"] = c.PostForm("_gift_card_custom_amount") == "yes"
	p.Meta["_gift_card_min"] = gxAtof(c.PostForm("_gift_card_min"))
	p.Meta["_gift_card_max"] = gxAtof(c.PostForm("_gift_card_max"))
	p.Meta["_gift_card_designs"] = strings.Join(gcParseDesigns(c.PostForm("_gift_card_designs")), ", ")
	p.Meta["_gift_card_expiration_days"] = float64(gxAtoi(c.PostForm("_gift_card_expiration_days")))
	p.Virtual, p.ManageStock, p.Backorders = true, false, "no"
	// The catalog price is the smallest amount.
	lo := 0.0
	if all := gcAmounts(p); len(all) > 0 {
		lo = all[0]
	} else if mn := gxNum(p.Meta["_gift_card_min"]); mn > 0 {
		lo = mn
	}
	p.RegularPrice, p.SalePrice = &lo, nil
}

// gcMaskCode hides all but the last four characters of a gift card code.
func gcMaskCode(code string) string {
	r := []rune(code)
	for i := 0; i < len(r)-4; i++ {
		if r[i] != '-' {
			r[i] = 'X'
		}
	}
	return string(r)
}

// gcCardsForEmail is used by the admin and the tests: the cards sent to an address.
func (s *Server) gcCardsForEmail(ctx context.Context, email string) []*gcCard {
	return s.gcQuery(ctx, `lower(recipient) = lower($1) ORDER BY id`, email)
}

var _ = strconv.Itoa
