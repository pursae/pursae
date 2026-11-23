package server

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Goocommerce › Status (page=status&tab=status|tools|action-scheduler|logs): the system status report,
// the maintenance tools, the scheduled actions (the jobs of Goocommerce) and logs.

type gooStatusItem struct {
	Label string
	Value template.HTML
	Plain string
	Help  string
}

type gooStatusSection struct {
	Title string
	Items []gooStatusItem
}

type gooTool struct {
	ID, Name, Button, Desc string
}

type gooJobRow struct {
	ID         int64
	Hook       string
	Status     string
	Args       string
	Recurrence string
	Scheduled  string
	Log        string
}

type gooStatusData struct {
	Tab      string
	Tabs     []gooOpt
	Sections []gooStatusSection
	Report   string
	Tools    []gooTool
	Nonce    string
	Jobs     []gooJobRow
	Views    []gooView
	Pager    gooPager
	Logs     []gooJobRow
}

var gooTools = []gooTool{
	{"clear_transients", "Pursae Commerce transients", "Clear transients", "This tool will clear the product/shop transients cache."},
	{"clear_expired_transients", "Expired transients", "Clear transients", "This tool will clear ALL expired transients from Pursae."},
	{"clear_expired_sessions", "Clear expired sessions", "Clear", "This tool will delete the expired customer sessions (carts) from the database."},
	{"clear_sessions", "Clear customer sessions", "Clear", "<strong class=\"red\">Note:</strong> This tool will delete all customer session data from the database, including current carts and saved carts in the database."},
	{"recount_terms", "Term counts", "Recount terms", "This tool will recount product terms - useful when changing your settings in a way which hides products from the catalog."},
	{"regenerate_product_lookup_tables", "Product lookup tables", "Regenerate", "This tool will regenerate the product lookup data: the active price of every product and the price range of variable and grouped products."},
	{"recalculate_ratings", "Recalculate product ratings", "Recalculate", "This tool will recalculate the average rating and rating count of every product from its approved reviews."},
	{"delete_orphaned_variations", "Delete orphaned variations", "Delete orphaned variations", "This tool will delete all variations which have no parent."},
	{"install_pages", "Create default Pursae Commerce pages", "Create pages", "This tool will install all the missing Pursae Commerce pages. Pages already defined and set up won't be replaced."},
	{"reset_roles", "Capabilities", "Reset capabilities", "This tool will reset the admin, customer and shop_manager roles to default. Use this if your users cannot access all of the Pursae Commerce admin pages."},
}

func (s *Server) gooAdminStatus(c *gin.Context) {
	tab := c.DefaultQuery("tab", "status")
	d := &gooStatusData{Tab: tab, Nonce: s.nonce(c, gooStoreNonce),
		Tabs: []gooOpt{{"status", "System status"}, {"tools", "Tools"}, {"logs", "Logs"}, {"action-scheduler", "Scheduled Actions"}}}
	var notices []notice
	switch tab {
	case "tools":
		d.Tools = gooTools
		if c.Request.Method == http.MethodPost || c.Query("action") != "" {
			if !s.checkNonce(c, gooStoreNonce) {
				s.gooDeny(c)
				return
			}
			action := c.PostForm("action")
			if action == "" {
				action = c.Query("action")
			}
			msg, err := s.gooRunTool(c, action)
			if err != nil {
				gooRedirect(c, gooAdminURL("status", "tab", "tools"), "", err.Error())
			} else {
				gooRedirect(c, gooAdminURL("status", "tab", "tools"), msg, "")
			}
			return
		}
	case "action-scheduler":
		s.gooJobsView(c, d)
	case "logs":
		s.gooLogsView(c, d)
	default:
		d.Tab = "status"
		d.Sections = s.gooSystemStatus(c)
		var b strings.Builder
		b.WriteString("### Pursae Commerce System Status Report ###\n")
		for _, sec := range d.Sections {
			b.WriteString("\n### " + sec.Title + " ###\n\n")
			for _, it := range sec.Items {
				b.WriteString(it.Label + ": " + it.Plain + "\n")
			}
		}
		d.Report = b.String()
	}
	s.gooStoreRender(c, "goocommerce?page=status", "wc-status", "Status", "admin/goo-status", d, notices...)
}

func gooYes(ok bool) (template.HTML, string) {
	if ok {
		return `<mark class="yes"><span class="dashicons dashicons-yes"></span></mark>`, "✔"
	}
	return `<mark class="no">&ndash;</mark>`, "–"
}

func (s *Server) gooSystemStatus(c *gin.Context) []gooStatusSection {
	ctx := c.Request.Context()
	item := func(label, v string) gooStatusItem { return gooStatusItem{Label: label, Value: gooH(v), Plain: v} }
	flag := func(label string, ok bool) gooStatusItem {
		h, p := gooYes(ok)
		return gooStatusItem{Label: label, Value: h, Plain: p}
	}
	home := s.links(ctx).HomeURL("/")
	env := gooStatusSection{Title: "Pursae environment", Items: []gooStatusItem{
		item("Pursae address (URL)", home), item("Site address (URL)", home),
		item("Pursae Commerce version", gooVersion), item("Pursae version", Version),
		item("Language", defaultStr(s.settings.String("WPLANG"), "en_US")), item("Timezone", s.loc().String()),
		flag("Pursae Commerce REST API", true),
		item("Uploads directory", s.cfg.UploadsDir),
	}}
	srv := gooStatusSection{Title: "Server environment", Items: []gooStatusItem{
		item("Server info", "Pursae (Go "+runtime.Version()+")"), item("Operating system", runtime.GOOS+"/"+runtime.GOARCH),
		item("CPUs", strconv.Itoa(runtime.NumCPU())), item("Goroutines", strconv.Itoa(runtime.NumGoroutine())),
	}}
	var pgVersion, dbSize string
	_ = s.store.Pool.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion)
	_ = s.store.Pool.QueryRow(ctx, `SELECT pg_size_pretty(pg_database_size(current_database()))`).Scan(&dbSize)
	db := gooStatusSection{Title: "Database", Items: []gooStatusItem{item("Database server", "PostgreSQL "+pgVersion), item("Total database size", dbSize)}}
	rows, err := s.store.Pool.Query(ctx, `SELECT c.relname, pg_size_pretty(pg_total_relation_size(c.oid)), coalesce(c.reltuples,0)::bigint FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind = 'r' AND c.relname LIKE 'goo\_%' AND n.nspname = current_schema() ORDER BY c.relname`)
	if err == nil {
		for rows.Next() {
			var name, size string
			var tuples int64
			if rows.Scan(&name, &size, &tuples) == nil {
				db.Items = append(db.Items, item(name, "Size: "+size+" | Rows (estimated): "+strconv.FormatInt(max(tuples, 0), 10)))
			}
		}
		rows.Close()
	}
	plugins := gooStatusSection{Title: "Active plugins"}
	for _, p := range s.installedPlugins(c) {
		if p.Active {
			by := "by " + p.Author
			if p.Native {
				by += " (native)"
			}
			plugins.Items = append(plugins.Items, item(p.Name, by+" – "+p.Version))
		}
	}
	settings := gooStatusSection{Title: "Settings", Items: []gooStatusItem{
		flag("API enabled", true), flag("Force SSL", s.gooOptBool("force_ssl_checkout")),
		item("Currency", s.gooCurrency()+" ("+gooCurrencySymbol(s.gooCurrency())+")"), item("Currency position", s.gooOptString("currency_pos")),
		item("Thousand separator", s.gooOptString("price_thousand_sep")), item("Decimal separator", s.gooOptString("price_decimal_sep")),
		item("Number of decimals", strconv.Itoa(s.gooDecimals())), flag("Taxes enabled", s.gooTaxEnabled()), flag("Shipping enabled", s.gooShippingEnabled()),
		flag("Coupons enabled", s.gooCouponsEnabled()), flag("Manage stock", s.gooManageStock()),
	}}
	var taxonomies []string
	for _, t := range []string{"product_type", "product_cat", "product_tag", "product_shipping_class"} {
		taxonomies = append(taxonomies, t)
	}
	settings.Items = append(settings.Items, item("Taxonomies: Product taxonomies", strings.Join(taxonomies, ", ")))
	pages := gooStatusSection{Title: "Pursae Commerce pages"}
	for _, pg := range []struct{ Key, Label, Short string }{{"shop", "Shop base", ""}, {"cart", "Cart", "[goocommerce_cart]"},
		{"checkout", "Checkout", "[goocommerce_checkout]"}, {"myaccount", "My account", "[goocommerce_my_account]"}, {"terms", "Terms and conditions", ""}} {
		id := s.gooPageID(pg.Key)
		if id == 0 {
			v := "Page not set"
			pages.Items = append(pages.Items, gooStatusItem{Label: pg.Label, Value: template.HTML(`<mark class="error"><span class="dashicons dashicons-warning"></span> ` + v + `</mark>`), Plain: "❌ " + v})
			continue
		}
		p, err := s.store.GetPost(ctx, id)
		if err != nil {
			pages.Items = append(pages.Items, gooStatusItem{Label: pg.Label, Value: `<mark class="error"><span class="dashicons dashicons-warning"></span> Page ID is set, but the page does not exist</mark>`, Plain: "❌ Page does not exist"})
			continue
		}
		v := "#" + itoa64(id) + " - " + p.Title
		h := template.HTMLEscapeString(v)
		plain := v
		if pg.Short != "" {
			if blk := "woocommerce/" + pg.Key; (pg.Key == "cart" || pg.Key == "checkout") && gooPageHasStoreBlock(pg.Key, p.Content) {
				h += ` <mark class="yes">` + template.HTMLEscapeString(blk) + ` &#10004;</mark>`
				plain += " ✔ " + blk
			} else if strings.Contains(p.Content, pg.Short) {
				h += ` <mark class="yes">` + template.HTMLEscapeString(pg.Short) + ` &#10004;</mark>`
				plain += " ✔ " + pg.Short
			} else {
				h += ` <mark class="error"><span class="dashicons dashicons-warning"></span> Page does not contain the ` + template.HTMLEscapeString(pg.Short) + ` shortcode.</mark>`
				plain += " ❌ missing " + pg.Short
			}
		}
		pages.Items = append(pages.Items, gooStatusItem{Label: pg.Label, Value: template.HTML(h), Plain: plain})
	}
	theme := gooStatusSection{Title: "Theme"}
	if t := s.activeTheme(); t != nil {
		theme.Items = append(theme.Items, item("Name", t.Name), item("Version", t.Version), item("Author", t.Author))
	}
	theme.Items = append(theme.Items, flag("Pursae Commerce support", true))
	return []gooStatusSection{env, srv, db, plugins, settings, pages, theme}
}

// gooRunTool runs a Status › Tools entry.
func (s *Server) gooRunTool(c *gin.Context, action string) (string, error) {
	ctx := c.Request.Context()
	switch action {
	case "clear_transients":
		_, err := s.store.Pool.Exec(ctx, `DELETE FROM transients WHERE key LIKE 'goo%' OR key LIKE 'wc\_%' OR key LIKE '\_wc\_%'`)
		s.purgeAll(ctx)
		return "Product transients cleared", err
	case "clear_expired_transients":
		tag, err := s.store.Pool.Exec(ctx, `DELETE FROM transients WHERE expires_at IS NOT NULL AND expires_at < now()`)
		return fmt.Sprintf("%d transients rows cleared", tag.RowsAffected()), err
	case "clear_expired_sessions":
		return "Expired sessions deleted.", s.gooCleanupSessions(ctx)
	case "clear_sessions":
		_, err := s.store.Pool.Exec(ctx, `DELETE FROM goo_sessions`)
		return "Deleted all active sessions.", err
	case "recount_terms":
		_, err := s.store.Pool.Exec(ctx, `UPDATE terms t SET post_count = (SELECT count(*) FROM post_terms pt JOIN posts p ON p.id = pt.post_id
			WHERE pt.term_id = t.id AND p.status = 'publish') WHERE t.taxonomy IN ('product_cat','product_tag','product_shipping_class') OR t.taxonomy LIKE 'pa\_%'`)
		return "Terms successfully recounted", err
	case "regenerate_product_lookup_tables":
		n := 0
		rows, err := s.store.Pool.Query(ctx, `SELECT post_id FROM goo_products WHERE type IN ('variable','grouped')`)
		if err != nil {
			return "", err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			p, err := s.gooGetProduct(ctx, id)
			if err != nil {
				continue
			}
			if p.Type == "variable" {
				err = s.gooSyncVariable(ctx, p)
			} else {
				err = s.gooSyncGrouped(ctx, p)
			}
			if err == nil {
				n++
			}
		}
		s.purgeAll(ctx)
		return fmt.Sprintf("Lookup tables are regenerated (%d variable and grouped products synced).", n), nil
	case "recalculate_ratings":
		rows, err := s.store.Pool.Query(ctx, `SELECT post_id FROM goo_products WHERE type <> 'variation'`)
		if err != nil {
			return "", err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			s.gooUpdateRating(ctx, id)
		}
		return fmt.Sprintf("Product ratings recalculated for %d products.", len(ids)), nil
	case "delete_orphaned_variations":
		tag, err := s.store.Pool.Exec(ctx, `DELETE FROM posts p WHERE p.type = 'product_variation' AND (p.parent_id IS NULL OR NOT EXISTS (SELECT 1 FROM posts pp WHERE pp.id = p.parent_id AND pp.type = 'product'))`)
		return fmt.Sprintf("%d orphaned variations deleted", tag.RowsAffected()), err
	case "install_pages":
		var uid int64
		if u := currentUser(c); u != nil {
			uid = u.ID
		}
		return "All missing Pursae Commerce pages successfully installed", s.gooCreatePages(ctx, uid)
	case "reset_roles":
		caps := `{"manage_woocommerce":true,"view_woocommerce_reports":true,"edit_products":true,"edit_others_products":true,"publish_products":true,"read_private_products":true,"delete_products":true,"delete_others_products":true,"delete_published_products":true,"delete_private_products":true,"edit_published_products":true,"edit_private_products":true,"manage_product_terms":true,"edit_shop_orders":true,"edit_shop_coupons":true}`
		if _, err := s.store.Pool.Exec(ctx, `UPDATE roles SET capabilities = capabilities || $1::jsonb WHERE slug IN ('administrator','shop_manager')`, caps); err != nil {
			return "", err
		}
		_, _ = s.store.Pool.Exec(ctx, `INSERT INTO roles (slug, name, is_system, capabilities) VALUES ('customer', 'Customer', false, '{"read":true,"level_0":true}') ON CONFLICT (slug) DO NOTHING`)
		_, _ = s.store.Pool.Exec(ctx, `INSERT INTO roles (slug, name, is_system, capabilities) SELECT 'shop_manager', 'Shop manager', false, capabilities || $1::jsonb
			FROM roles WHERE slug = 'editor' ON CONFLICT (slug) DO NOTHING`, caps)
		return "Roles successfully reset", nil
	}
	return "", fmt.Errorf("There was an error calling %s", action)
}

// Scheduled actions ------------------------------------------------------------------------------------------------

func (s *Server) gooJobsView(c *gin.Context, d *gooStatusData) {
	ctx := c.Request.Context()
	status := c.Query("status")
	dbStatus := map[string]string{"pending": "queued", "complete": "done", "failed": "failed", "in-progress": "running"}
	counts := map[string]int{}
	rows, err := s.store.Pool.Query(ctx, `SELECT status, count(*) FROM jobs WHERE name LIKE 'goocommerce%' OR name LIKE 'goo\_%' GROUP BY status`)
	total := 0
	if err == nil {
		for rows.Next() {
			var st string
			var n int
			if rows.Scan(&st, &n) == nil {
				counts[st] = n
				total += n
			}
		}
		rows.Close()
	}
	d.Views = append(d.Views, gooView{Label: "All", URL: gooAdminURL("status", "tab", "action-scheduler"), Count: total, Current: status == ""})
	for _, v := range []gooOpt{{"pending", "Pending"}, {"in-progress", "In-progress"}, {"complete", "Complete"}, {"failed", "Failed"}} {
		if n := counts[dbStatus[v.Value]]; n > 0 {
			d.Views = append(d.Views, gooView{Label: v.Label, URL: gooAdminURL("status", "tab", "action-scheduler", "status", v.Value), Count: n, Current: status == v.Value})
		}
	}
	where := `(name LIKE 'goocommerce%' OR name LIKE 'goo\_%')`
	args := []any{}
	if st := dbStatus[status]; st != "" {
		where += ` AND status = $1`
		args = append(args, st)
	}
	page := gooPaged(c)
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE `+where, args...).Scan(&n)
	d.Pager = newGooPager(gooAdminURL("status", "tab", "action-scheduler", "status", status), page, 20, n)
	d.Jobs = s.gooJobRows(ctx, `SELECT id, name, status, payload::text, recurrence, run_at, last_error FROM jobs WHERE `+where+
		fmt.Sprintf(` ORDER BY run_at DESC, id DESC LIMIT 20 OFFSET %d`, (page-1)*20), args...)
}

func (s *Server) gooJobRows(ctx context.Context, sql string, args ...any) []gooJobRow {
	rows, err := s.store.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []gooJobRow
	for rows.Next() {
		var r gooJobRow
		var at time.Time
		if rows.Scan(&r.ID, &r.Hook, &r.Status, &r.Args, &r.Recurrence, &at, &r.Log) != nil {
			continue
		}
		r.Status = map[string]string{"queued": "Pending", "done": "Complete", "failed": "Failed", "running": "In-progress"}[r.Status]
		r.Scheduled = at.In(s.loc()).Format("2006-01-02 15:04:05 -0700")
		if r.Recurrence == "" {
			r.Recurrence = "Non-repeating"
		} else {
			r.Recurrence = "Every " + strings.TrimSuffix(r.Recurrence, "ly")
			r.Recurrence = strings.ReplaceAll(r.Recurrence, "dai", "day")
		}
		out = append(out, r)
	}
	return out
}

// gooLogsView lists failed jobs and webhooks with failed deliveries.
func (s *Server) gooLogsView(c *gin.Context, d *gooStatusData) {
	ctx := c.Request.Context()
	d.Logs = s.gooJobRows(ctx, `SELECT id, name, status, payload::text, recurrence, updated_at, last_error FROM jobs
		WHERE (name LIKE 'goocommerce%' OR name LIKE 'goo\_%') AND last_error <> '' ORDER BY updated_at DESC LIMIT 100`)
	rows, err := s.store.Pool.Query(ctx, `SELECT id, name, topic, delivery_url, failures, created_at FROM goo_webhooks WHERE failures > 0 ORDER BY id DESC`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r gooJobRow
		var topic, urlStr string
		var failures int
		var at time.Time
		if rows.Scan(&r.ID, &r.Hook, &topic, &urlStr, &failures, &at) == nil {
			r.Status = "Failed"
			r.Args = topic + " → " + urlStr
			r.Log = fmt.Sprintf("Webhook delivery failed %d times.", failures)
			r.Scheduled = at.In(s.loc()).Format("2006-01-02 15:04:05")
			d.Logs = append(d.Logs, r)
		}
	}
}
