package server

import (
	"context"
	"strconv"
	"strings"
)

// Goocommerce's options. Every WooCommerce option keeps its name without the "woocommerce_" prefix
// and is stored as the site option "goocommerce_<name>" (woocommerce_currency → goocommerce_currency).
// The settings screens are declared here like WooCommerce's settings API (WC_Settings_Page::get_settings):
// tabs, sections and fields; the admin renders and saves them generically.

const gooOptPrefix = "goocommerce_"

// gooField is one setting (a WC settings API array: id, title, type, desc, options, default).
type gooField struct {
	ID      string   // option name without the prefix
	Title   string   // label
	Type    string   // text, textarea, number, select, multiselect, checkbox, radio, color, email, password, single_select_page, single_select_country, multi_select_countries, title, sectionend, price, image
	Desc    string   // description below the field (HTML allowed: trusted, written here)
	Tip     string   // help tip
	Default any      // the default value: string, bool, int
	Options []gooOpt // select / radio / multiselect choices
	Group   string   // checkboxgroup: "start", "" or "end" (several checkboxes under one title)
	Show    string   // show_if_checked: the id of the checkbox this field depends on
	Min     string   // number: custom_attributes min / step
	Step    string
	Class   string // wc-enhanced-select, short, …
	Holder  string // placeholder
}

type gooOpt struct{ Value, Label string }

// gooSection is a section of a settings tab (?tab=products&section=inventory).
type gooSection struct {
	ID, Title string
	Fields    []gooField
}

// gooTab is a settings tab (?tab=general).
type gooTab struct {
	ID, Title string
	Sections  []gooSection
}

var gooWeightUnits = []gooOpt{{"kg", "kg"}, {"g", "g"}, {"lbs", "lbs"}, {"oz", "oz"}}
var gooDimensionUnits = []gooOpt{{"m", "m"}, {"cm", "cm"}, {"mm", "mm"}, {"in", "in"}, {"yd", "yd"}}

// gooSettingsTabs is the WooCommerce › Settings screen: General, Products, Tax, Shipping, Payments,
// Accounts & Privacy, Emails, Integration and Advanced. Shipping zones, payment gateways, tax rates and
// e-mails have their own sub-screens (rendered by the admin) besides these option fields.
func gooSettingsTabs() []gooTab {
	currencies := make([]gooOpt, 0, len(gooCurrencyOrder))
	for _, c := range gooCurrencyOrder {
		currencies = append(currencies, gooOpt{c.Code, c.Name + " (" + c.Symbol + ") — " + c.Code})
	}
	return []gooTab{
		{ID: "general", Title: "General", Sections: []gooSection{{ID: "", Fields: []gooField{
			{ID: "store_address_title", Title: "Store Address", Type: "title", Desc: "This is where your business is located. Tax rates and shipping rates will use this address."},
			{ID: "store_address", Title: "Address line 1", Type: "text", Default: ""},
			{ID: "store_city", Title: "City", Type: "text", Default: ""},
			{ID: "default_country", Title: "Country / State", Type: "single_select_country", Default: "US:CA", Desc: "The country and state or province, if any, in which your business is located."},
			{ID: "store_postcode", Title: "Postcode / ZIP", Type: "text", Default: ""},
			{ID: "store_address_end", Type: "sectionend"},
			{ID: "general_options", Title: "General options", Type: "title"},
			{ID: "allowed_countries", Title: "Selling location(s)", Type: "select", Default: "all", Desc: "This option lets you limit which countries you are willing to sell to.",
				Options: []gooOpt{{"all", "Sell to all countries"}, {"all_except", "Sell to all countries, except for…"}, {"specific", "Sell to specific countries"}}},
			{ID: "all_except_countries", Title: "Sell to all countries, except for…", Type: "multi_select_countries", Default: []string{}},
			{ID: "specific_allowed_countries", Title: "Sell to specific countries", Type: "multi_select_countries", Default: []string{}},
			{ID: "ship_to_countries", Title: "Shipping location(s)", Type: "select", Default: "", Desc: "Choose which countries you want to ship to, or choose to ship to all locations you sell to.",
				Options: []gooOpt{{"", "Ship to all countries you sell to"}, {"all", "Ship to all countries"}, {"specific", "Ship to specific countries only"}, {"disabled", "Disable shipping &amp; shipping calculations"}}},
			{ID: "specific_ship_to_countries", Title: "Ship to specific countries", Type: "multi_select_countries", Default: []string{}},
			{ID: "default_customer_address", Title: "Default customer location", Type: "select", Default: "base",
				Desc:    "This option determines a customer's default location. The default location is your store's base address.",
				Options: []gooOpt{{"", "No location by default"}, {"base", "Shop country/region"}, {"geolocation", "Geolocate"}}},
			{ID: "calc_taxes", Title: "Enable taxes", Type: "checkbox", Default: false, Desc: "Enable tax rates and calculations", Tip: "Rates will be configurable and taxes will be calculated during checkout."},
			{ID: "enable_coupons", Title: "Enable coupons", Type: "checkbox", Default: true, Desc: "Enable the use of coupon codes", Group: "start", Tip: "Coupons can be applied from the cart and checkout pages."},
			{ID: "calc_discounts_sequentially", Title: "", Type: "checkbox", Default: false, Desc: "Calculate coupon discounts sequentially", Group: "end", Show: "enable_coupons",
				Tip: "When applying multiple coupons, apply the first coupon to the full price and the second coupon to the discounted price and so on."},
			{ID: "general_options_end", Type: "sectionend"},
			{ID: "pricing_options", Title: "Currency options", Type: "title", Desc: "The following options affect how prices are displayed on the frontend."},
			{ID: "currency", Title: "Currency", Type: "select", Default: "USD", Options: currencies, Class: "wc-enhanced-select"},
			{ID: "currency_pos", Title: "Currency position", Type: "select", Default: "left",
				Options: []gooOpt{{"left", "Left"}, {"right", "Right"}, {"left_space", "Left with space"}, {"right_space", "Right with space"}}},
			{ID: "price_thousand_sep", Title: "Thousand separator", Type: "text", Default: ",", Class: "short"},
			{ID: "price_decimal_sep", Title: "Decimal separator", Type: "text", Default: ".", Class: "short"},
			{ID: "price_num_decimals", Title: "Number of decimals", Type: "number", Default: 2, Min: "0", Step: "1", Class: "short"},
			{ID: "pricing_options_end", Type: "sectionend"},
		}}}},
		{ID: "products", Title: "Products", Sections: []gooSection{
			{ID: "", Title: "General", Fields: []gooField{
				{ID: "shop_options", Title: "Shop pages", Type: "title"},
				{ID: "shop_page_id", Title: "Shop page", Type: "single_select_page", Default: 0, Desc: "The base page can also be used in your product permalinks."},
				{ID: "cart_redirect_after_add", Title: "Add to cart behaviour", Type: "checkbox", Default: false, Desc: "Redirect to the cart page after successful addition", Group: "start"},
				{ID: "enable_ajax_add_to_cart", Type: "checkbox", Default: true, Desc: "Enable AJAX add to cart buttons on archives", Group: "end"},
				{ID: "installments_on_product", Title: "Installment options", Type: "checkbox", Default: true,
					Desc: "Show the installment options of the bank payment methods on product pages (stores selling in TRY with an installment plan)"},
				{ID: "placeholder_image", Title: "Placeholder image", Type: "number", Default: 0, Desc: "This is the attachment ID, or image URL, used for placeholder images in the product catalog. Products with no image will use this."},
				{ID: "shop_options_end", Type: "sectionend"},
				{ID: "product_measurement_options", Title: "Measurements", Type: "title"},
				{ID: "weight_unit", Title: "Weight unit", Type: "select", Default: "kg", Options: gooWeightUnits, Desc: "This controls what unit you will define weights in."},
				{ID: "dimension_unit", Title: "Dimensions unit", Type: "select", Default: "cm", Options: gooDimensionUnits, Desc: "This controls what unit you will define lengths in."},
				{ID: "product_measurement_options_end", Type: "sectionend"},
				{ID: "product_rating_options", Title: "Reviews", Type: "title"},
				{ID: "enable_reviews", Title: "Enable reviews", Type: "checkbox", Default: true, Desc: "Enable product reviews", Group: "start"},
				{ID: "review_rating_verification_label", Type: "checkbox", Default: true, Desc: "Show \"verified owner\" label on customer reviews", Show: "enable_reviews"},
				{ID: "review_rating_verification_required", Type: "checkbox", Default: false, Desc: "Reviews can only be left by \"verified owners\"", Group: "end", Show: "enable_reviews"},
				{ID: "enable_review_rating", Title: "Product ratings", Type: "checkbox", Default: true, Desc: "Enable star rating on reviews", Group: "start", Show: "enable_reviews"},
				{ID: "review_rating_required", Type: "checkbox", Default: true, Desc: "Star ratings should be required, not optional", Group: "end", Show: "enable_review_rating"},
				{ID: "product_rating_options_end", Type: "sectionend"},
			}},
			{ID: "inventory", Title: "Inventory", Fields: []gooField{
				{ID: "product_inventory_options", Title: "Inventory", Type: "title"},
				{ID: "manage_stock", Title: "Manage stock", Type: "checkbox", Default: true, Desc: "Enable stock management"},
				{ID: "hold_stock_minutes", Title: "Hold stock (minutes)", Type: "number", Default: 60, Min: "0", Step: "1", Desc: "Hold stock (for unpaid orders) for x minutes. When this limit is reached, the pending order will be cancelled. Leave blank to disable."},
				{ID: "notify_low_stock", Title: "Notifications", Type: "checkbox", Default: true, Desc: "Enable low stock notifications", Group: "start"},
				{ID: "notify_no_stock", Type: "checkbox", Default: true, Desc: "Enable out of stock notifications", Group: "end"},
				{ID: "stock_email_recipient", Title: "Notification recipient(s)", Type: "text", Default: "", Desc: "Enter recipients (comma separated) that will receive this notification."},
				{ID: "notify_low_stock_amount", Title: "Low stock threshold", Type: "number", Default: 2, Min: "0", Step: "1"},
				{ID: "notify_no_stock_amount", Title: "Out of stock threshold", Type: "number", Default: 0, Step: "1", Desc: "When product stock reaches this amount the stock status will change to \"out of stock\"."},
				{ID: "hide_out_of_stock_items", Title: "Out of stock visibility", Type: "checkbox", Default: false, Desc: "Hide out of stock items from the catalog"},
				{ID: "stock_format", Title: "Stock display format", Type: "select", Default: "",
					Options: []gooOpt{{"", "Always show quantity remaining in stock e.g. \"12 in stock\""}, {"low_amount", "Only show quantity remaining in stock when low e.g. \"Only 2 left in stock\""}, {"no_amount", "Never show quantity remaining in stock"}}},
				{ID: "product_inventory_options_end", Type: "sectionend"},
			}},
			{ID: "downloadable", Title: "Downloadable products", Fields: []gooField{
				{ID: "digital_download_options", Title: "Downloadable products", Type: "title"},
				{ID: "file_download_method", Title: "File download method", Type: "select", Default: "force",
					Options: []gooOpt{{"force", "Force downloads"}, {"xsendfile", "X-Accel-Redirect/X-Sendfile"}, {"redirect", "Redirect only (Insecure)"}}},
				{ID: "downloads_require_login", Title: "Access restriction", Type: "checkbox", Default: false, Desc: "Downloads require login", Group: "start"},
				{ID: "downloads_grant_access_after_payment", Type: "checkbox", Default: true, Desc: "Grant access to downloadable products after payment", Group: "end"},
				{ID: "digital_download_options_end", Type: "sectionend"},
			}},
		}},
		{ID: "tax", Title: "Tax", Sections: []gooSection{{ID: "", Title: "Tax options", Fields: []gooField{
			{ID: "tax_options", Title: "Tax options", Type: "title"},
			{ID: "prices_include_tax", Title: "Prices entered with tax", Type: "radio", Default: "no",
				Options: []gooOpt{{"yes", "Yes, I will enter prices inclusive of tax"}, {"no", "No, I will enter prices exclusive of tax"}}},
			{ID: "tax_based_on", Title: "Calculate tax based on", Type: "select", Default: "shipping",
				Options: []gooOpt{{"shipping", "Customer shipping address"}, {"billing", "Customer billing address"}, {"base", "Shop base address"}}},
			{ID: "shipping_tax_class", Title: "Shipping tax class", Type: "select", Default: "inherit", Desc: "Optionally control which tax class shipping gets, or leave it so shipping tax is based on the cart items themselves.",
				Options: []gooOpt{{"inherit", "Shipping tax class based on cart items"}, {"", "Standard"}, {"reduced-rate", "Reduced rate"}, {"zero-rate", "Zero rate"}}},
			{ID: "tax_round_at_subtotal", Title: "Rounding", Type: "checkbox", Default: false, Desc: "Round tax at subtotal level, instead of rounding per line"},
			{ID: "tax_classes", Title: "Additional tax classes", Type: "textarea", Default: "Reduced rate\nZero rate", Desc: "List additional tax classes you need below (1 per line, e.g. Reduced Rates). These are in addition to \"Standard rate\" which exists by default."},
			{ID: "tax_display_shop", Title: "Display prices in the shop", Type: "select", Default: "excl", Options: []gooOpt{{"incl", "Including tax"}, {"excl", "Excluding tax"}}},
			{ID: "tax_display_cart", Title: "Display prices during cart and checkout", Type: "select", Default: "excl", Options: []gooOpt{{"incl", "Including tax"}, {"excl", "Excluding tax"}}},
			{ID: "price_display_suffix", Title: "Price display suffix", Type: "text", Default: "", Holder: "N/A", Desc: "Define text to show after your product prices. This could be, for example, \"inc. Vat\" to explain your pricing. You can also have prices substituted here using one of the following: <code>{price_including_tax}</code>, <code>{price_excluding_tax}</code>."},
			{ID: "tax_total_display", Title: "Display tax totals", Type: "select", Default: "itemized", Options: []gooOpt{{"single", "As a single total"}, {"itemized", "Itemized"}}},
			{ID: "tax_options_end", Type: "sectionend"},
		}}}},
		{ID: "shipping", Title: "Shipping", Sections: []gooSection{
			{ID: "", Title: "Shipping zones"},
			{ID: "options", Title: "Shipping settings", Fields: []gooField{
				{ID: "shipping_options", Title: "Shipping settings", Type: "title"},
				{ID: "enable_shipping_calc", Title: "Calculations", Type: "checkbox", Default: true, Desc: "Enable the shipping calculator on the cart page", Group: "start"},
				{ID: "shipping_cost_requires_address", Type: "checkbox", Default: false, Desc: "Hide shipping costs until an address is entered", Group: "end"},
				{ID: "ship_to_destination", Title: "Shipping destination", Type: "radio", Default: "billing",
					Options: []gooOpt{{"shipping", "Default to customer shipping address"}, {"billing", "Default to customer billing address"}, {"billing_only", "Force shipping to the customer billing address"}}},
				{ID: "shipping_options_end", Type: "sectionend"},
			}},
			{ID: "classes", Title: "Classes"},
		}},
		{ID: "checkout", Title: "Payments", Sections: []gooSection{{ID: "", Title: "Payment methods"}}},
		{ID: "account", Title: "Accounts &amp; Privacy", Sections: []gooSection{{ID: "", Fields: []gooField{
			{ID: "account_registration_options", Title: "", Type: "title"},
			{ID: "enable_guest_checkout", Title: "Guest checkout", Type: "checkbox", Default: true, Desc: "Allow customers to place orders without an account", Group: "start"},
			{ID: "enable_checkout_login_reminder", Type: "checkbox", Default: true, Desc: "Allow customers to log into an existing account during checkout", Group: "end"},
			{ID: "enable_signup_and_login_from_checkout", Title: "Account creation", Type: "checkbox", Default: true, Desc: "Allow customers to create an account during checkout", Group: "start"},
			{ID: "enable_myaccount_registration", Type: "checkbox", Default: false, Desc: "Allow customers to create an account on the \"My account\" page"},
			{ID: "registration_generate_username", Type: "checkbox", Default: true, Desc: "When creating an account, automatically generate an account username for the customer based on their name, surname or email"},
			{ID: "registration_generate_password", Type: "checkbox", Default: true, Desc: "When creating an account, send the new user a link to set their password", Group: "end"},
			{ID: "account_registration_options_end", Type: "sectionend"},
			{ID: "account_erasure_options", Title: "Account erasure requests", Type: "title"},
			{ID: "erasure_request_removes_order_data", Title: "Account erasure requests", Type: "checkbox", Default: false, Desc: "Remove personal data from orders on request", Group: "start"},
			{ID: "erasure_request_removes_download_data", Type: "checkbox", Default: false, Desc: "Remove access to downloads on request", Group: "end"},
			{ID: "account_erasure_options_end", Type: "sectionend"},
			{ID: "privacy_policy_options", Title: "Privacy policy", Type: "title", Desc: "This section controls the display of your website privacy policy. The privacy notices below will not show up unless a privacy page is set."},
			{ID: "registration_privacy_policy_text", Title: "Registration privacy policy", Type: "textarea", Default: "Your personal data will be used to support your experience throughout this website, to manage access to your account, and for other purposes described in our [privacy_policy]."},
			{ID: "checkout_privacy_policy_text", Title: "Checkout privacy policy", Type: "textarea", Default: "Your personal data will be used to process your order, support your experience throughout this website, and for other purposes described in our [privacy_policy]."},
			{ID: "privacy_policy_options_end", Type: "sectionend"},
			{ID: "personal_data_retention", Title: "Personal data retention", Type: "title", Desc: "Choose how long to retain personal data when it's no longer needed for processing. Leave the following options blank to retain this data indefinitely."},
			{ID: "delete_inactive_accounts", Title: "Retain inactive accounts", Type: "number", Default: 0, Min: "0", Desc: "months (0 = keep)"},
			{ID: "trash_pending_orders", Title: "Retain pending orders", Type: "number", Default: 0, Min: "0", Desc: "days (0 = keep)"},
			{ID: "trash_failed_orders", Title: "Retain failed orders", Type: "number", Default: 0, Min: "0", Desc: "days (0 = keep)"},
			{ID: "trash_cancelled_orders", Title: "Retain cancelled orders", Type: "number", Default: 0, Min: "0", Desc: "days (0 = keep)"},
			{ID: "anonymize_completed_orders", Title: "Retain completed orders", Type: "number", Default: 0, Min: "0", Desc: "months (0 = keep)"},
			{ID: "personal_data_retention_end", Type: "sectionend"},
		}}}},
		{ID: "email", Title: "Emails", Sections: []gooSection{{ID: "", Title: "Email notifications", Fields: []gooField{
			{ID: "email_sender_options", Title: "Email sender options", Type: "title", Desc: "Set the name and email address you'd like your outgoing emails to use."},
			{ID: "email_from_name", Title: "\"From\" name", Type: "text", Default: ""},
			{ID: "email_from_address", Title: "\"From\" address", Type: "email", Default: ""},
			{ID: "email_sender_options_end", Type: "sectionend"},
			{ID: "weekly_summary_options", Title: "Store summary", Type: "title", Desc: "A weekly e-mail with last week's sales, sent to the stock notification recipient(s) on the first run after Monday begins."},
			{ID: "weekly_summary_email", Title: "Weekly summary", Type: "checkbox", Default: false, Desc: "Send a weekly store summary e-mail"},
			{ID: "weekly_summary_options_end", Type: "sectionend"},
			{ID: "email_template_options", Title: "Email template", Type: "title", Desc: "This section lets you customize the emails sent from your store."},
			{ID: "email_header_image", Title: "Header image", Type: "text", Default: "", Holder: "N/A", Desc: "Paste the URL of an image you want to show in the email header."},
			{ID: "email_footer_text", Title: "Footer text", Type: "textarea", Default: "{site_title} &mdash; Built with Pursae", Desc: "The text to appear in the footer of all emails. Available placeholders: {site_title} {site_url}"},
			{ID: "email_base_color", Title: "Base color", Type: "color", Default: "#7f54b3"},
			{ID: "email_background_color", Title: "Background color", Type: "color", Default: "#f7f7f7"},
			{ID: "email_body_background_color", Title: "Body background color", Type: "color", Default: "#ffffff"},
			{ID: "email_text_color", Title: "Body text color", Type: "color", Default: "#3c3c3c"},
			{ID: "email_template_options_end", Type: "sectionend"},
		}}}},
		{ID: "advanced", Title: "Advanced", Sections: []gooSection{
			{ID: "", Title: "Page setup", Fields: []gooField{
				{ID: "advanced_page_options", Title: "Page setup", Type: "title", Desc: "These pages need to be set so that the store knows where to send users to checkout."},
				{ID: "cart_page_id", Title: "Cart page", Type: "single_select_page", Default: 0, Desc: "Page contents: [goocommerce_cart]"},
				{ID: "checkout_page_id", Title: "Checkout page", Type: "single_select_page", Default: 0, Desc: "Page contents: [goocommerce_checkout]"},
				{ID: "myaccount_page_id", Title: "My account page", Type: "single_select_page", Default: 0, Desc: "Page contents: [goocommerce_my_account]"},
				{ID: "terms_page_id", Title: "Terms and conditions", Type: "single_select_page", Default: 0, Desc: "If you define a \"Terms\" page the customer will be asked if they accept them when checking out."},
				{ID: "force_ssl_checkout", Title: "Secure checkout", Type: "checkbox", Default: false, Desc: "Force secure checkout"},
				{ID: "advanced_page_options_end", Type: "sectionend"},
				{ID: "checkout_endpoint_options", Title: "Checkout endpoints", Type: "title", Desc: "Endpoints are appended to your page URLs to handle specific actions during the checkout process. They should be unique."},
				{ID: "checkout_pay_endpoint", Title: "Pay", Type: "text", Default: "order-pay"},
				{ID: "checkout_order_received_endpoint", Title: "Order received", Type: "text", Default: "order-received"},
				{ID: "checkout_endpoint_options_end", Type: "sectionend"},
				{ID: "account_endpoint_options", Title: "Account endpoints", Type: "title", Desc: "Endpoints are appended to your page URLs to handle specific actions on the accounts pages. They should be unique and can be left blank to disable the endpoint."},
				{ID: "myaccount_orders_endpoint", Title: "Orders", Type: "text", Default: "orders"},
				{ID: "myaccount_view_order_endpoint", Title: "View order", Type: "text", Default: "view-order"},
				{ID: "myaccount_downloads_endpoint", Title: "Downloads", Type: "text", Default: "downloads"},
				{ID: "myaccount_edit_account_endpoint", Title: "Edit account", Type: "text", Default: "edit-account"},
				{ID: "myaccount_edit_address_endpoint", Title: "Addresses", Type: "text", Default: "edit-address"},
				{ID: "myaccount_payment_methods_endpoint", Title: "Payment methods", Type: "text", Default: "payment-methods"},
				{ID: "myaccount_lost_password_endpoint", Title: "Lost password", Type: "text", Default: "lost-password"},
				{ID: "logout_endpoint", Title: "Logout", Type: "text", Default: "customer-logout"},
				{ID: "account_endpoint_options_end", Type: "sectionend"},
			}},
			{ID: "keys", Title: "REST API"},
			{ID: "webhooks", Title: "Webhooks"},
			{ID: "features", Title: "Features", Fields: []gooField{
				{ID: "features_options", Title: "Features", Type: "title", Desc: "Start using new features that are being progressively rolled out to improve the store management experience."},
				{ID: "analytics_enabled", Title: "Analytics", Type: "checkbox", Default: true, Desc: "Enable Analytics"},
				{ID: "feature_order_attribution", Title: "Order Attribution", Type: "checkbox", Default: true, Desc: "Enable this feature to track and credit channels and campaigns that contribute to orders on your site"},
				{ID: "features_options_end", Type: "sectionend"},
			}},
		}},
	}
}

// gooOptDefaults are the defaults of every declared field plus the internal options.
var gooOptDefaults = func() map[string]any {
	m := map[string]any{
		"version":                            "",
		"db_version":                         "",
		"product_base":                       "product", // Settings › Permalinks › Product base
		"category_base":                      "product-category",
		"tag_base":                           "product-tag",
		"attribute_base":                     "",
		"queue_flush":                        false,
		"catalog_columns":                    4, // Customizer › Product Catalog
		"catalog_rows":                       4,
		"default_catalog_orderby":            "menu_order",
		"shop_page_display":                  "", // "", subcategories, both
		"category_archive_display":           "",
		"thumbnail_cropping":                 "1:1",
		"demo_store":                         false, // store notice
		"demo_store_notice":                  "This is a demo store for testing purposes &mdash; no orders shall be fulfilled.",
		"checkout_company_field":             "optional", // Customizer › Checkout
		"checkout_address_2_field":           "optional",
		"checkout_phone_field":               "required",
		"checkout_highlight_required_fields": true,
		"extensions":                         map[string]any{}, // Phase 2 modules switched on (goo_extensions.go)
	}
	for _, t := range gooSettingsTabs() {
		for _, sec := range t.Sections {
			for _, f := range sec.Fields {
				if f.Type != "title" && f.Type != "sectionend" {
					m[f.ID] = f.Default
				}
			}
		}
	}
	return m
}()

// gooOpt is get_option('woocommerce_<key>') with the declared default.
func (s *Server) gooOpt(key string) any {
	if v := s.settings.Get(gooOptPrefix + key); v != nil {
		return v
	}
	return gooOptDefaults[key]
}

func (s *Server) gooOptString(key string) string {
	switch v := s.gooOpt(key).(type) {
	case string:
		return v
	case nil:
		return ""
	case bool:
		if v {
			return "yes"
		}
		return "no"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	}
	return ""
}

func (s *Server) gooOptBool(key string) bool {
	switch v := s.gooOpt(key).(type) {
	case bool:
		return v
	case string:
		return v == "yes" || v == "1" || v == "true"
	case float64:
		return v != 0
	case int:
		return v != 0
	}
	return false
}

func (s *Server) gooOptInt(key string) int {
	switch v := s.gooOpt(key).(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	case bool:
		if v {
			return 1
		}
	}
	return 0
}

func (s *Server) gooOptList(key string) []string {
	switch v := s.gooOpt(key).(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if str, ok := x.(string); ok {
				out = append(out, str)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return strings.Split(v, ",")
	}
	return nil
}

// gooOptMap is a JSON object option (gateway settings, e-mail settings, extensions).
func (s *Server) gooOptMap(key string) map[string]any {
	if m, ok := s.gooOpt(key).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// gooSetOpts saves options by their short names.
func (s *Server) gooSetOpts(ctx context.Context, values map[string]any) error {
	m := make(map[string]any, len(values))
	pages := false
	for k, v := range values {
		m[gooOptPrefix+k] = s.gooSealGatewaySecrets(k, v)
		pages = pages || strings.HasSuffix(k, "_page_id")
	}
	if err := s.settings.Set(ctx, m); err != nil {
		return err
	}
	if pages && s.gooOn() {
		s.configureCache() // the cart, checkout and account pages are never cached
	}
	return nil
}

// gooTaxClasses are the tax classes: "" (Standard) plus the additional classes, as slug → name.
func (s *Server) gooTaxClasses() []gooOpt {
	out := []gooOpt{{"", "Standard"}}
	for _, line := range strings.Split(s.gooOptString("tax_classes"), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		out = append(out, gooOpt{gooSanitizeKey(name), name})
	}
	return out
}

// gooSanitizeKey is sanitize_title() for internal keys: lowercase, dashes.
func gooSanitizeKey(v string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(v)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// gooBaseCountry / gooBaseState split the default_country option ("US:CA").
func (s *Server) gooBaseCountry() string {
	c, _, _ := strings.Cut(s.gooOptString("default_country"), ":")
	return c
}

func (s *Server) gooBaseState() string {
	_, st, _ := strings.Cut(s.gooOptString("default_country"), ":")
	return st
}

// gooAllowedCountries is WC_Countries::get_allowed_countries() (code → name).
func (s *Server) gooAllowedCountries() []gooNamed {
	switch s.gooOptString("allowed_countries") {
	case "specific":
		return gooNamedFromCodes(s.gooOptList("specific_allowed_countries"))
	case "all_except":
		ex := map[string]bool{}
		for _, c := range s.gooOptList("all_except_countries") {
			ex[c] = true
		}
		var out []gooNamed
		for _, c := range gooCountryOrder {
			if !ex[c.Code] {
				out = append(out, c)
			}
		}
		return out
	}
	return gooCountryOrder
}

// gooShippingCountries is WC_Countries::get_shipping_countries().
func (s *Server) gooShippingCountries() []gooNamed {
	switch s.gooOptString("ship_to_countries") {
	case "all":
		return gooCountryOrder
	case "specific":
		return gooNamedFromCodes(s.gooOptList("specific_ship_to_countries"))
	case "disabled":
		return nil
	}
	return s.gooAllowedCountries()
}

func gooNamedFromCodes(codes []string) []gooNamed {
	var out []gooNamed
	for _, c := range codes {
		if n, ok := gooCountries[c]; ok {
			out = append(out, gooNamed{c, n})
		}
	}
	return out
}

// gooShippingEnabled is wc_shipping_enabled().
func (s *Server) gooShippingEnabled() bool { return s.gooOptString("ship_to_countries") != "disabled" }

// gooTaxEnabled is wc_tax_enabled().
func (s *Server) gooTaxEnabled() bool { return s.gooOptBool("calc_taxes") }

// gooPricesIncludeTax is wc_prices_include_tax().
func (s *Server) gooPricesIncludeTax() bool {
	return s.gooTaxEnabled() && s.gooOptString("prices_include_tax") == "yes"
}

// gooCouponsEnabled is wc_coupons_enabled().
func (s *Server) gooCouponsEnabled() bool { return s.gooOptBool("enable_coupons") }

// gooManageStock is the store-wide "Enable stock management" switch.
func (s *Server) gooManageStock() bool { return s.gooOptBool("manage_stock") }
