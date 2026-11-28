package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	gomail "wpgo/internal/mail"
	"wpgo/internal/netguard"
)

// Admin screens of WPGO Mail SMTP: Settings (General, Email Log, Alerts, Additional Connections,
// Smart Routing, Email Controls, Misc), Email Log, Email Reports, Tools (Email Test, Export, Debug
// Events) and the Setup Wizard, all under /wp-admin/admin?page=wp-mail-smtp*.

const msBase = "/wp-admin/admin?page="

var msSettingsTabs = [][2]string{{"general", "General"}, {"logs", "Email Log"}, {"alerts", "Alerts"}, {"connections", "Additional Connections"},
	{"routing", "Smart Routing"}, {"control", "Email Controls"}, {"misc", "Misc"}}

var msToolsTabs = [][2]string{{"test", "Email Test"}, {"export", "Export"}, {"debug-events", "Debug Events"}}

var msRetention = [][2]string{{"0", "Forever"}, {"1", "1 Day"}, {"7", "1 Week"}, {"30", "1 Month"}, {"90", "3 Months"}, {"180", "6 Months"}, {"365", "1 Year"}}

var msRuleProps = [][2]string{{"subject", "Subject"}, {"message", "Message"}, {"from_email", "From Email"}, {"from_name", "From Name"},
	{"to", "To"}, {"reply_to", "Reply-To"}, {"initiator", "Initiator"}}

var msRuleOps = [][2]string{{"contains", "Contains"}, {"not_contains", "Does not contain"}, {"is", "Is"}, {"is_not", "Is not"},
	{"starts", "Starts with"}, {"ends", "Ends with"}, {"regex", "Regex match"}, {"not_regex", "Does not match regex"}}

type msFieldView struct {
	msField
	Name, Value, Locked string
	Checked, HasSecret  bool
	Choices             []choice
}

type msMailerView struct {
	msMailerDef
	Selected              bool
	Fields                []msFieldView
	OAuthUser, ConnectURL string
	RemoveURL             string
	Connected, CanConnect bool
	ConnID, RedirectURI   string
}

type msConnView struct {
	ID, Name, FromEmail, FromName, Mailer string
	Mailers                               []msMailerView
	Primary                               bool
}

type msScreen struct {
	Page, Tab, Nonce, ActionNonce string
	Tabs                          []choice
	O                             *msOptions
	Locked                        map[string]string
	Conn                          *msConnView
	Connections                   []msConn
	EditConn                      bool
	Backups                       []choice
	Retention, DebugRetention     []choice
	Controls                      []msControlGroup
	Routes                        []msRouteView
	RuleProps, RuleOps, RouteConn []choice
	AlertsLocked                  bool
	TwilioHasToken                bool
	AlertEmail                    [3]string
	AlertSlack, AlertDiscord      [3]string
	AlertTeams, AlertWebhook      [3]string
	Thresholds                    []choice
	// Email Log
	Logs                []*msLogEntry
	Entry               *msLogEntry
	Filter              msLogFilter
	Total, Paged, Pages int
	CountAll, CountSent int
	CountFailed         int
	PrevURL, NextURL    string
	// Reports
	Stats     *msStats
	Days      int
	Chart     template.HTML
	RangeOpts []choice
	// Tools
	TestTo, TestConn, TestResult, TestDebug string
	TestOK, TestHTML                        bool
	TestHints                               []string
	ConnOpts                                []choice
	Events                                  []msEvent
	DomainCheck                             []msDNSCheck
	WebhookURLs                             [][2]string // Email Log settings: mailer title, bounce webhook URL
	// Wizard
	Step, ErrorText string
	WizardMailers   []msMailerView
	RedirectURI     string
}

type msControlGroup struct {
	Title string
	Items []choice
}

type msRouteView struct {
	Index      int
	Connection string
	Enabled    bool
	Groups     [][]msRule
}

type msEvent struct {
	ID                       int64
	Type, Content, Initiator string
	Date                     string
}

type msDNSCheck struct {
	Name, Status, Detail string // Status: ok, warning, error
}

// mailSMTPAdmin serves every admin?page=wp-mail-smtp* screen.
func (s *Server) mailSMTPAdmin(c *gin.Context, page string) {
	u := currentUser(c)
	if !u.Can("manage_options", nil) || !s.mailSMTPOn() {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	switch page {
	case "wp-mail-smtp":
		s.msSettingsScreen(c)
	case "wp-mail-smtp-logs":
		s.msLogsScreen(c)
	case "wp-mail-smtp-reports":
		s.msReportsScreen(c)
	case "wp-mail-smtp-tools":
		s.msToolsScreen(c)
	case "wp-mail-smtp-setup-wizard":
		s.msWizard(c)
	default: // an unregistered page, as admin.php answers it
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
	}
}

func msChoices(opts [][2]string, cur string) []choice {
	out := make([]choice, 0, len(opts))
	for _, o := range opts {
		out = append(out, choice{Value: o[0], Label: o[1], Checked: o[0] == cur})
	}
	return out
}

func (s *Server) msTabs(page string, tabs [][2]string, cur string) []choice {
	var out []choice
	for _, t := range tabs {
		out = append(out, choice{Value: msBase + page + "&tab=" + t[0], Label: t[1], Checked: t[0] == cur})
	}
	return out
}

func (s *Server) msRender(c *gin.Context, d *msScreen, title string, notices ...notice) {
	s.adminPage(c, "admin?page="+d.Page, title, "wp-mail-smtp-page "+d.Page, "admin/mailsmtp", d, notices...)
}

func msBack(c *gin.Context, to string) { c.Redirect(http.StatusFound, to) }

// --- connection forms (General tab, Additional Connections, wizard) --------------------------

// msMailerViews builds the mailer sections of a connection form.
func (s *Server) msMailerViews(c *gin.Context, o *msOptions, connID, selected string, opts func(mailer string) map[string]string, lock bool) []msMailerView {
	var out []msMailerView
	for _, d := range msMailers {
		mv := msMailerView{msMailerDef: d, Selected: d.Slug == selected}
		vals := opts(d.Slug)
		for _, f := range d.Fields {
			fv := msFieldView{msField: f, Name: d.Slug + "__" + f.Key}
			v, ok := vals[f.Key]
			if !ok {
				v = f.Default
			}
			if lock {
				fv.Locked = o.locked[d.Slug+"."+f.Key]
			}
			switch {
			case f.Type == "password" || msSecretKeys[f.Key]:
				fv.HasSecret = v != ""
			case f.Type == "checkbox":
				fv.Checked = v != "" && v != "0"
			default:
				fv.Value = v
			}
			if len(f.Options) > 0 {
				fv.Choices = msChoices(f.Options, v)
			}
			mv.Fields = append(mv.Fields, fv)
		}
		mv.ConnID = connID
		if d.OAuth {
			mv.RedirectURI = s.msRedirectURI()
			mv.Connected = vals["refresh_token"] != ""
			mv.OAuthUser = vals["oauth_user"]
			mv.CanConnect = vals["client_id"] != "" && vals["client_secret"] != ""
			n := s.nonce(c, "wp-mail-smtp-oauth")
			mv.ConnectURL = msBase + "wp-mail-smtp&action=oauth_connect&conn=" + connID + "&mailer=" + d.Slug + "&_wpnonce=" + n
			mv.RemoveURL = msBase + "wp-mail-smtp&action=oauth_remove&conn=" + connID + "&mailer=" + d.Slug + "&_wpnonce=" + n
		}
		out = append(out, mv)
	}
	return out
}

// msParseMailer reads one mailer's fields from a form into opts (secrets sealed, blank secrets
// kept, "Remove" clears them). It reports whether any field of the mailer was in the form.
func (s *Server) msParseMailer(c *gin.Context, o *msOptions, mailer string, opts map[string]string, lock bool) bool {
	d := msMailerDefBySlug(mailer)
	if d == nil {
		return false
	}
	seen := false
	for _, f := range d.Fields {
		name := mailer + "__" + f.Key
		if lock && o.locked[mailer+"."+f.Key] != "" {
			continue
		}
		raw, present := c.GetPostForm(name)
		if f.Type == "checkbox" {
			if _, marker := c.GetPostForm(name + "__present"); !marker && !present {
				continue
			}
			seen = true
			opts[f.Key] = map[bool]string{true: "1", false: ""}[present && raw != "" && raw != "0"]
			continue
		}
		if f.Type == "password" || msSecretKeys[f.Key] {
			if c.PostForm(name+"__remove") != "" {
				delete(opts, f.Key)
				seen = true
			} else if v := strings.TrimSpace(raw); v != "" {
				opts[f.Key] = s.msSeal(v)
				seen = true
			}
			continue
		}
		if present {
			seen = true
			opts[f.Key] = strings.TrimSpace(raw)
		}
	}
	return seen
}

func msValidMailer(m string) bool { return msMailerDefBySlug(m) != nil }

// --- Settings --------------------------------------------------------------------------------

func (s *Server) msSettingsScreen(c *gin.Context) {
	tab := c.DefaultQuery("tab", "general")
	switch c.Query("action") {
	case "oauth_connect", "oauth_remove":
		s.msOAuthAction(c)
		return
	case "delete_connection":
		if !s.checkNonce(c, "wp-mail-smtp-connections") {
			s.denyNonce(c)
			return
		}
		o := s.msLoadRaw()
		id := c.Query("conn")
		out := o.Connections[:0]
		for _, cn := range o.Connections {
			if cn.ID != id {
				out = append(out, cn)
			}
		}
		o.Connections = out
		if o.Backup == id {
			o.Backup = ""
		}
		_ = s.msSave(c, o)
		msBack(c, msBase+"wp-mail-smtp&tab=connections&message=deleted")
		return
	}
	if tab == "auth" {
		s.msOAuthCallback(c)
		return
	}
	if c.Request.Method == http.MethodPost {
		s.msSaveSettings(c, tab)
		return
	}
	o := s.msLoad()
	d := &msScreen{Page: "wp-mail-smtp", Tab: tab, O: o, Locked: o.locked, Nonce: s.nonce(c, "wp-mail-smtp-"+tab),
		Tabs: s.msTabs("wp-mail-smtp", msSettingsTabs, tab), RedirectURI: s.msRedirectURI()}
	if d.Locked == nil {
		d.Locked = map[string]string{}
	}
	var notices []notice
	if c.Query("message") == "deleted" {
		notices = append(notices, notice{Type: "success", Message: "Connection deleted.", Dismissible: true})
	}
	if e := c.Query("oauth_error"); e != "" {
		notices = append(notices, notice{Type: "error", Message: template.HTML("<strong>OAuth:</strong> " + template.HTMLEscapeString(e)), Dismissible: true})
	}
	if c.Query("oauth") == "connected" {
		notices = append(notices, notice{Type: "success", Message: "You have successfully linked the current site with your account. Now you can start sending emails through it.", Dismissible: true})
	}
	switch tab {
	case "general":
		p := o.primary()
		d.Conn = &msConnView{ID: "primary", Primary: true, FromEmail: p.FromEmail, FromName: p.FromName, Mailer: p.Mailer,
			Mailers: s.msMailerViews(c, o, "primary", p.Mailer, func(m string) map[string]string { return o.Mailers[m] }, true)}
		d.Backups = []choice{{Value: "", Label: "None", Checked: o.Backup == ""}}
		for _, cn := range o.Connections {
			d.Backups = append(d.Backups, choice{Value: cn.ID, Label: cn.Name + " (" + msMailerTitle(cn.Mailer) + ")", Checked: o.Backup == cn.ID})
		}
	case "logs":
		d.Retention = msChoices(msRetention, strconv.Itoa(o.Logs.Retention))
		home := strings.TrimRight(s.links(c).Home, "/")
		for _, m := range [][2]string{{"postmark", "Postmark"}, {"sendgrid", "SendGrid"}, {"mailgun", "Mailgun"}, {"amazonses", "Amazon SES (HTTPS subscription of the SNS topic that receives bounces, complaints and deliveries)"}} {
			d.WebhookURLs = append(d.WebhookURLs, [2]string{m[1], home + msTrackBase + "/webhook/" + m[0] + "?token=" + s.msWebhookToken()})
		}
	case "alerts":
		copy(d.AlertEmail[:], o.Alerts.Email.URLs)
		copy(d.AlertSlack[:], o.Alerts.Slack.URLs)
		copy(d.AlertDiscord[:], o.Alerts.Discord.URLs)
		copy(d.AlertTeams[:], o.Alerts.Teams.URLs)
		copy(d.AlertWebhook[:], o.Alerts.Webhook.URLs)
		d.TwilioHasToken = o.Alerts.Twilio.Token != ""
		d.Thresholds = msChoices([][2]string{{"0", "Every failed email"}, {"2", "2 failures in 30 minutes"}, {"3", "3 failures in 30 minutes"},
			{"5", "5 failures in 30 minutes"}, {"10", "10 failures in 30 minutes"}}, strconv.Itoa(o.Alerts.Threshold))
	case "connections":
		d.Connections = o.Connections
		for i := range d.Connections {
			d.Connections[i].Title = msMailerTitle(d.Connections[i].Mailer)
		}
		d.ActionNonce = s.nonce(c, "wp-mail-smtp-connections")
		if id := c.Query("edit"); id != "" {
			d.EditConn = true
			cn := &msConn{ID: "new", Mailer: "smtp"}
			if id != "new" {
				if found := o.conn(id); found != nil && id != "primary" {
					cn = found
				}
			}
			d.Conn = &msConnView{ID: cn.ID, Name: cn.Name, FromEmail: cn.FromEmail, FromName: cn.FromName, Mailer: cn.Mailer,
				Mailers: s.msMailerViews(c, o, cn.ID, cn.Mailer, func(m string) map[string]string {
					if m == cn.Mailer {
						return cn.Opts
					}
					return nil
				}, false)}
		}
	case "routing":
		for i, r := range o.SmartRouting.Routes {
			d.Routes = append(d.Routes, msRouteView{Index: i, Connection: r.Connection, Enabled: r.Enabled, Groups: r.Groups})
		}
		d.RuleProps, d.RuleOps = msChoices(msRuleProps, ""), msChoices(msRuleOps, "")
		for _, cn := range o.Connections {
			d.RouteConn = append(d.RouteConn, choice{Value: cn.ID, Label: cn.Name})
		}
	case "control":
		var cur *msControlGroup
		for _, ec := range msEmailControls {
			if cur == nil || cur.Title != ec.Group {
				d.Controls = append(d.Controls, msControlGroup{Title: ec.Group})
				cur = &d.Controls[len(d.Controls)-1]
			}
			cur.Items = append(cur.Items, choice{Value: ec.Key, Label: ec.Label, Checked: !o.Controls[ec.Key]})
		}
	case "misc":
	default: // an unknown tab opens the first one, like saving one does
		msBack(c, msBase+"wp-mail-smtp")
		return
	}
	s.msRender(c, d, "Pursae SMTP Settings", notices...)
}

func formInt(c *gin.Context, k string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(c.PostForm(k)))
	return max(n, 0)
}

func formBool(c *gin.Context, k string) bool { v := c.PostForm(k); return v != "" && v != "0" }

func (s *Server) msSaveSettings(c *gin.Context, tab string) {
	if !s.checkNonce(c, "wp-mail-smtp-"+tab) {
		s.denyNonce(c)
		return
	}
	o := s.msLoadRaw()
	locked := s.msLoad().locked
	unlocked := func(k string) bool { return locked[k] == "" }
	back := msBase + "wp-mail-smtp&tab=" + tab
	switch tab {
	case "general":
		if unlocked("mail.from_email") {
			o.Mail.FromEmail = strings.TrimSpace(c.PostForm("from_email"))
		}
		if unlocked("mail.from_name") {
			o.Mail.FromName = strings.TrimSpace(c.PostForm("from_name"))
		}
		if unlocked("mail.from_email_force") {
			o.Mail.FromEmailForce = formBool(c, "from_email_force")
		}
		if unlocked("mail.from_name_force") {
			o.Mail.FromNameForce = formBool(c, "from_name_force")
		}
		if unlocked("mail.return_path") {
			o.Mail.ReturnPath = formBool(c, "return_path")
		}
		if m := c.PostForm("mailer"); unlocked("mail.mailer") && msValidMailer(m) {
			o.Mail.Mailer = m
		}
		if o.Mail.FromEmail != "" {
			if _, err := parseOneAddr(o.Mail.FromEmail); err != nil {
				msBack(c, back+"&error="+url.QueryEscape("Please enter a valid From Email address."))
				return
			}
		}
		for _, d := range msMailers {
			opts := o.mailerOpts(d.Slug)
			s.msParseMailer(c, o, d.Slug, opts, true)
			if len(opts) == 0 {
				delete(o.Mailers, d.Slug)
			}
		}
		o.Backup = c.PostForm("backup_connection")
		if o.conn(o.Backup) == nil || o.Backup == "primary" {
			o.Backup = ""
		}
	case "logs":
		o.Logs.Enabled = formBool(c, "logs_enabled")
		o.Logs.LogContent = formBool(c, "log_email_content")
		o.Logs.Retention = formInt(c, "log_retention_period")
		o.Logs.TrackOpens = formBool(c, "track_opens")
		o.Logs.TrackClicks = formBool(c, "track_clicks")
	case "alerts":
		o.Alerts.Threshold = formInt(c, "failed_threshold")
		hook := func(h *msHook, prefix string) {
			h.Enabled = formBool(c, prefix+"_enabled")
			h.URLs = nil
			for _, v := range c.PostFormArray(prefix + "[]") {
				if v = strings.TrimSpace(v); v != "" {
					h.URLs = append(h.URLs, v)
				}
			}
		}
		hook(&o.Alerts.Email, "alert_email")
		hook(&o.Alerts.Slack, "alert_slack_webhook")
		hook(&o.Alerts.Discord, "alert_discord_webhook")
		hook(&o.Alerts.Teams, "alert_teams_webhook")
		hook(&o.Alerts.Webhook, "alert_custom_webhook")
		for _, u := range append(append(append(o.Alerts.Slack.URLs, o.Alerts.Discord.URLs...), o.Alerts.Teams.URLs...), o.Alerts.Webhook.URLs...) {
			if err := msWebhookURL(u); err != nil {
				msBack(c, back+"&error="+url.QueryEscape(err.Error()))
				return
			}
		}
		t := &o.Alerts.Twilio
		t.Enabled = formBool(c, "alert_twilio_sms_enabled")
		t.SID = strings.TrimSpace(c.PostForm("twilio_account_sid"))
		t.From = strings.TrimSpace(c.PostForm("twilio_from"))
		t.To = strings.TrimSpace(c.PostForm("twilio_to"))
		if c.PostForm("twilio_auth_token__remove") != "" {
			t.Token = ""
		} else if v := strings.TrimSpace(c.PostForm("twilio_auth_token")); v != "" {
			t.Token = s.msSeal(v)
		}
	case "connections":
		id := c.PostForm("conn_id")
		cn := msConn{ID: id, Name: strings.TrimSpace(c.PostForm("conn_name")), FromEmail: strings.TrimSpace(c.PostForm("from_email")),
			FromName: strings.TrimSpace(c.PostForm("from_name")), Mailer: c.PostForm("mailer"), Opts: map[string]string{}}
		if !msValidMailer(cn.Mailer) || cn.Name == "" {
			msBack(c, back+"&edit="+url.QueryEscape(id)+"&error="+url.QueryEscape("Please enter a connection name and choose a mailer."))
			return
		}
		idx := -1
		for i := range o.Connections {
			if o.Connections[i].ID == id {
				idx = i
				if o.Connections[i].Mailer == cn.Mailer {
					for k, v := range o.Connections[i].Opts {
						cn.Opts[k] = v
					}
				}
			}
		}
		s.msParseMailer(c, o, cn.Mailer, cn.Opts, false)
		if idx < 0 {
			cn.ID = strconv.FormatInt(time.Now().UnixNano(), 36)
			o.Connections = append(o.Connections, cn)
		} else {
			o.Connections[idx] = cn
		}
		back += "&edit=" + cn.ID
	case "routing":
		o.SmartRouting.Enabled = formBool(c, "smart_routing_enabled")
		o.SmartRouting.Routes = msParseRoutes(c)
	case "control":
		for _, ec := range msEmailControls {
			o.Controls[ec.Key] = !formBool(c, "control_"+ec.Key)
		}
	case "misc":
		if unlocked("general.do_not_send") {
			o.General.DoNotSend = formBool(c, "do_not_send")
		}
		// "Hide Announcements" has no control any more (Pursae shows none); the stored value is kept for import/export.
		o.General.HideErrors = formBool(c, "email_delivery_errors_hidden")
		o.General.HideWidget = formBool(c, "dashboard_widget_hidden")
		if unlocked("general.summary_report_email_disabled") {
			o.General.SummaryDisabled = formBool(c, "summary_report_email_disabled")
		}
		o.General.Uninstall = formBool(c, "uninstall")
		r := &o.RateLimit
		r.Enabled = formBool(c, "rate_limit_enabled")
		r.Minute, r.Hour, r.Day, r.Week, r.Month = formInt(c, "rate_minute"), formInt(c, "rate_hour"), formInt(c, "rate_day"), formInt(c, "rate_week"), formInt(c, "rate_month")
	default:
		msBack(c, msBase+"wp-mail-smtp")
		return
	}
	if err := s.msSave(c, o); err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	msBack(c, back+"&settings-updated=true")
}

func parseOneAddr(v string) (string, error) {
	a := parseAddrList([]string{v})
	if len(a) != 1 {
		return "", fmt.Errorf("invalid address")
	}
	return a[0].Address, nil
}

var msRouteKey = regexp.MustCompile(`^routes\[(\d+)\]\[g\]\[(\d+)\]\[(\d+)\]\[(property|operator|value)\]$`)

// msParseRoutes reads routes[R][connection|enabled] and routes[R][g][G][N][property|operator|value].
func msParseRoutes(c *gin.Context) []msRoute {
	_ = c.Request.ParseForm()
	type key struct{ r, g, n int }
	rules := map[key]*msRule{}
	routes := map[int]*msRoute{}
	get := func(r int) *msRoute {
		if routes[r] == nil {
			routes[r] = &msRoute{}
		}
		return routes[r]
	}
	for k, vals := range c.Request.PostForm {
		v := ""
		if len(vals) > 0 {
			v = strings.TrimSpace(vals[0])
		}
		if m := msRouteKey.FindStringSubmatch(k); m != nil {
			r, _ := strconv.Atoi(m[1])
			g, _ := strconv.Atoi(m[2])
			n, _ := strconv.Atoi(m[3])
			kk := key{r, g, n}
			if rules[kk] == nil {
				rules[kk] = &msRule{}
			}
			switch m[4] {
			case "property":
				rules[kk].Property = v
			case "operator":
				rules[kk].Operator = v
			default:
				rules[kk].Value = v
			}
			get(r)
			continue
		}
		var r int
		var field string
		if n, _ := fmt.Sscanf(k, "routes[%d][%s", &r, &field); n == 2 {
			switch strings.TrimSuffix(field, "]") {
			case "connection":
				get(r).Connection = v
			case "enabled":
				get(r).Enabled = v != ""
			}
		}
	}
	keys := make([]key, 0, len(rules))
	for k := range rules {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.r != b.r {
			return a.r < b.r
		}
		if a.g != b.g {
			return a.g < b.g
		}
		return a.n < b.n
	})
	groups := map[int]map[int][]msRule{}
	for _, k := range keys {
		rl := rules[k]
		if rl.Property == "" || rl.Operator == "" {
			continue
		}
		if groups[k.r] == nil {
			groups[k.r] = map[int][]msRule{}
		}
		groups[k.r][k.g] = append(groups[k.r][k.g], *rl)
	}
	var idx []int
	for r := range routes {
		idx = append(idx, r)
	}
	sort.Ints(idx)
	var out []msRoute
	for _, r := range idx {
		rt := routes[r]
		var gi []int
		for g := range groups[r] {
			gi = append(gi, g)
		}
		sort.Ints(gi)
		for _, g := range gi {
			rt.Groups = append(rt.Groups, groups[r][g])
		}
		if rt.Connection != "" && len(rt.Groups) > 0 {
			out = append(out, *rt)
		}
	}
	return out
}

// --- OAuth -----------------------------------------------------------------------------------

func (s *Server) msOAuthAction(c *gin.Context) {
	if !s.checkNonce(c, "wp-mail-smtp-oauth") {
		s.denyNonce(c)
		return
	}
	connID, mailer := c.Query("conn"), c.Query("mailer")
	back := msBase + "wp-mail-smtp"
	if connID != "primary" {
		back += "&tab=connections&edit=" + url.QueryEscape(connID)
	}
	o := s.msLoad()
	cn := o.conn(connID)
	if cn == nil || !msOAuthMailer(mailer) {
		msBack(c, back)
		return
	}
	if connID == "primary" {
		cn.Mailer, cn.Opts = mailer, o.Mailers[mailer]
	}
	if c.Query("action") == "oauth_remove" {
		raw := s.msLoadRaw()
		var opts map[string]string
		if connID == "primary" {
			opts = raw.Mailers[mailer]
		} else if rc := raw.conn(connID); rc != nil {
			opts = rc.Opts
		}
		for _, k := range []string{"access_token", "refresh_token", "expires", "oauth_user", "account_id"} {
			delete(opts, k)
		}
		_ = s.msSave(c, raw)
		msBack(c, back)
		return
	}
	state := connID + "." + mailer + "." + s.nonce(c, "wp-mail-smtp-oauth-state")
	c.Redirect(http.StatusFound, s.msOAuth(cn).AuthCodeURL(state))
}

// msOAuthCallback is the redirect URI: it exchanges the code and stores the tokens.
func (s *Server) msOAuthCallback(c *gin.Context) {
	parts := strings.SplitN(c.Query("state"), ".", 3)
	if len(parts) != 3 || !s.checkNonceValue(c, parts[2], "wp-mail-smtp-oauth-state") {
		s.denyNonce(c)
		return
	}
	connID, mailer := parts[0], parts[1]
	back := msBase + "wp-mail-smtp"
	if connID != "primary" {
		back += "&tab=connections&edit=" + url.QueryEscape(connID)
	}
	if e := c.Query("error"); e != "" {
		msBack(c, back+"&oauth_error="+url.QueryEscape(e+": "+c.Query("error_description")))
		return
	}
	o := s.msLoad()
	cn := o.conn(connID)
	if cn == nil || !msOAuthMailer(mailer) {
		msBack(c, back)
		return
	}
	if connID == "primary" {
		cn.Mailer, cn.Opts = mailer, o.Mailers[mailer]
	}
	oa := s.msOAuth(cn)
	tok, err := oa.Exchange(c, c.Query("code"))
	if err != nil {
		msBack(c, back+"&oauth_error="+url.QueryEscape(err.Error()))
		return
	}
	user, account, err := oa.Account(c, tok.AccessToken)
	if err != nil {
		msBack(c, back+"&oauth_error="+url.QueryEscape(err.Error()))
		return
	}
	s.msStoreToken(c, connID, mailer, tok, user, account)
	msBack(c, back+"&oauth=connected")
}

// --- Email Log -------------------------------------------------------------------------------

const msPerPage = 20

func (s *Server) msLogsScreen(c *gin.Context) {
	ctx := c.Request.Context()
	if c.Request.Method == http.MethodPost || c.Query("action") != "" {
		s.msLogsAction(c)
		return
	}
	o := s.msLoad()
	d := &msScreen{Page: "wp-mail-smtp-logs", O: o, Nonce: s.nonce(c, "wp-mail-smtp-logs")}
	var notices []notice
	if m := c.Query("message"); m != "" {
		typ := "success"
		if c.Query("failed") == "1" {
			typ = "error"
		}
		notices = append(notices, notice{Type: typ, Message: template.HTML(template.HTMLEscapeString(m)), Dismissible: true})
	}
	if !o.Logs.Enabled {
		notices = append(notices, notice{Type: "warning", Message: template.HTML(`Email logging is disabled. <a href="` + msBase + `wp-mail-smtp&amp;tab=logs">Enable it</a> to keep a record of the emails your site sends.`)})
	}
	if c.Query("mode") == "view" {
		id, _ := strconv.ParseInt(c.Query("email_id"), 10, 64)
		e, err := s.msLogGet(ctx, id)
		if err != nil {
			c.String(http.StatusNotFound, "Email log entry not found.")
			return
		}
		d.Entry = e
		s.msRender(c, d, "Email Log", notices...)
		return
	}
	d.Filter = msLogFilter{Status: c.Query("status"), Search: strings.TrimSpace(c.Query("s")), From: c.Query("date_from"), To: c.Query("date_to")}
	d.Paged = max(1, atoiDefault(c.Query("paged"), 1))
	logs, total, err := s.msLogList(ctx, d.Filter, msPerPage, (d.Paged-1)*msPerPage)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	d.Logs, d.Total = logs, total
	d.Pages = max(1, (total+msPerPage-1)/msPerPage)
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'sent'), count(*) FILTER (WHERE status = 'failed') FROM wpmailsmtp_emails_log`).
		Scan(&d.CountAll, &d.CountSent, &d.CountFailed)
	q := c.Request.URL.Query()
	if d.Paged > 1 {
		q.Set("paged", strconv.Itoa(d.Paged-1))
		d.PrevURL = "/wp-admin/admin?" + q.Encode()
	}
	if d.Paged < d.Pages {
		q.Set("paged", strconv.Itoa(d.Paged+1))
		d.NextURL = "/wp-admin/admin?" + q.Encode()
	}
	s.msRender(c, d, "Email Log", notices...)
}

func atoiDefault(v string, def int) int {
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

func (s *Server) msLogsAction(c *gin.Context) {
	if !s.checkNonce(c, "wp-mail-smtp-logs") {
		s.denyNonce(c)
		return
	}
	ctx := c.Request.Context()
	back := msBase + "wp-mail-smtp-logs"
	action := c.PostForm("action")
	if action == "" || action == "-1" {
		action = c.PostForm("action2")
	}
	if action == "" {
		action = c.Query("action")
	}
	var ids []int64
	for _, v := range append(c.PostFormArray("email_id[]"), c.Query("email_id"), c.PostForm("email_id")) {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	done := func(msg string, failed bool) {
		u := back + "&message=" + url.QueryEscape(msg)
		if failed {
			u += "&failed=1"
		}
		msBack(c, u)
	}
	switch action {
	case "delete":
		tag, err := s.store.Pool.Exec(ctx, `DELETE FROM wpmailsmtp_emails_log WHERE id = ANY($1)`, ids)
		if err != nil {
			done(err.Error(), true)
			return
		}
		done(fmt.Sprintf("%d email log entries deleted.", tag.RowsAffected()), false)
	case "delete_all":
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM wpmailsmtp_emails_log`)
		done("All email log entries were deleted.", false)
	case "resend":
		var to []string
		if r := strings.TrimSpace(c.PostForm("recipients")); r != "" {
			to = splitList(r)
			for _, a := range to {
				if _, err := parseOneAddr(a); err != nil {
					done("Invalid recipient: "+a, true)
					return
				}
			}
		}
		sent, failed := 0, 0
		var lastErr error
		for _, id := range ids {
			e, err := s.msLogGet(ctx, id)
			if err == nil {
				err = s.msResend(ctx, e, to)
			}
			if err != nil {
				failed++
				lastErr = err
			} else {
				sent++
			}
		}
		if failed > 0 {
			done(fmt.Sprintf("%d email(s) resent, %d failed: %v", sent, failed, lastErr), true)
			return
		}
		done(fmt.Sprintf("%d email(s) resent successfully.", sent), false)
	default:
		msBack(c, back)
	}
}

// --- Email Reports ---------------------------------------------------------------------------

func (s *Server) msReportsScreen(c *gin.Context) {
	days := atoiDefault(c.Query("days"), 7)
	if days != 7 && days != 14 && days != 30 && days != 90 {
		days = 7
	}
	st, err := s.msReport(c.Request.Context(), days)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	o := s.msLoad()
	d := &msScreen{Page: "wp-mail-smtp-reports", O: o, Stats: st, Days: days, Chart: msChart(st)}
	for _, n := range []int{7, 14, 30, 90} {
		d.RangeOpts = append(d.RangeOpts, choice{Value: msBase + "wp-mail-smtp-reports&days=" + strconv.Itoa(n), Label: fmt.Sprintf("Last %d days", n), Checked: n == days})
	}
	s.msRender(c, d, "Email Reports")
}

// msChart draws the sent/failed bar chart of the reports screen and the dashboard widget.
func msChart(st *msStats) template.HTML {
	const w, h, pad = 640.0, 180.0, 24.0
	n := float64(len(st.Days))
	if n == 0 {
		return ""
	}
	top := float64(max(st.Max, 1))
	bw := (w - pad) / n
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="wpms-chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="Emails sent and failed per day" preserveAspectRatio="none" style="width:100%%;height:auto">`, w, h+28)
	for i := 0; i <= 4; i++ {
		y := pad/2 + (h-pad/2)*float64(i)/4
		fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" stroke="#dcdcde" stroke-width="1"/>`, pad, w, y, y)
	}
	fmt.Fprintf(&b, `<text x="0" y="%.0f" font-size="10" fill="#646970">%d</text>`, pad/2+4, st.Max)
	for i, d := range st.Days {
		x := pad + float64(i)*bw + bw*0.15
		bwInner := bw * 0.7
		hs := (h - pad/2) * float64(d.Sent) / top
		hf := (h - pad/2) * float64(d.Failed) / top
		base := h
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="#00a32a"><title>%s: %d sent</title></rect>`, x, base-hs, bwInner, hs, template.HTMLEscapeString(d.Label), d.Sent)
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="#d63638"><title>%s: %d failed</title></rect>`, x, base-hs-hf, bwInner, hf, template.HTMLEscapeString(d.Label), d.Failed)
		if len(st.Days) <= 14 || i%(len(st.Days)/10+1) == 0 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.0f" font-size="10" text-anchor="middle" fill="#646970">%s</text>`, x+bwInner/2, h+16, template.HTMLEscapeString(d.Label))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// --- Tools -----------------------------------------------------------------------------------

func (s *Server) msToolsScreen(c *gin.Context) {
	tab := c.DefaultQuery("tab", "test")
	o := s.msLoad()
	d := &msScreen{Page: "wp-mail-smtp-tools", Tab: tab, O: o, Tabs: s.msTabs("wp-mail-smtp-tools", msToolsTabs, tab), Nonce: s.nonce(c, "wp-mail-smtp-tools-"+tab)}
	if c.Request.Method == http.MethodPost || c.Query("action") != "" {
		if !s.checkNonce(c, "wp-mail-smtp-tools-"+tab) {
			s.denyNonce(c)
			return
		}
	}
	var notices []notice
	switch tab {
	case "test":
		d.TestTo, d.TestHTML, d.TestConn = s.settings.String("admin_email"), true, "primary"
		d.ConnOpts = []choice{{Value: "primary", Label: "Primary Connection (" + msMailerTitle(o.Mail.Mailer) + ")"}}
		for _, cn := range o.Connections {
			d.ConnOpts = append(d.ConnOpts, choice{Value: cn.ID, Label: cn.Name + " (" + msMailerTitle(cn.Mailer) + ")"})
		}
		if c.Request.Method == http.MethodPost {
			d.TestTo, d.TestHTML, d.TestConn = strings.TrimSpace(c.PostForm("wp-mail-smtp[test_email]")), formBool(c, "wp-mail-smtp[test_email_html]"), c.PostForm("wp-mail-smtp[test_connection]")
			s.msRunTest(c, o, d)
			if d.TestOK && c.PostForm("domain_check") != "" {
				d.DomainCheck = msCheckDomain(c, s.msTestFromDomain(o, d.TestConn), s.msTestMailer(o, d.TestConn), strings.TrimSpace(c.PostForm("dkim_selector")))
			}
		}
		for i := range d.ConnOpts {
			d.ConnOpts[i].Checked = d.ConnOpts[i].Value == d.TestConn
		}
	case "export":
		if c.Request.Method == http.MethodPost {
			s.msExport(c)
			return
		}
	case "debug-events":
		if c.Request.Method == http.MethodPost {
			raw := s.msLoadRaw()
			if c.PostForm("action") == "delete_all" {
				_, _ = s.store.Pool.Exec(c, `DELETE FROM wpmailsmtp_debug_events`)
			} else {
				raw.General.DebugSending = formBool(c, "debug_email_sending")
				raw.General.DebugRetention = formInt(c, "debug_events_retention_period")
				_ = s.msSave(c, raw)
			}
			msBack(c, msBase+"wp-mail-smtp-tools&tab=debug-events&settings-updated=true")
			return
		}
		d.DebugRetention = msChoices(msRetention, strconv.Itoa(o.General.DebugRetention))
		rows, err := s.store.Pool.Query(c, `SELECT id, event_type, content, initiator, created_at FROM wpmailsmtp_debug_events ORDER BY created_at DESC, id DESC LIMIT 100`)
		if err == nil {
			for rows.Next() {
				var e msEvent
				var t time.Time
				if rows.Scan(&e.ID, &e.Type, &e.Content, &e.Initiator, &t) == nil {
					e.Date = t.In(s.loc()).Format("Jan 2, 2006 3:04 pm")
					d.Events = append(d.Events, e)
				}
			}
			rows.Close()
		}
	default: // an unknown tab opens the first one
		msBack(c, msBase+"wp-mail-smtp-tools")
		return
	}
	s.msRender(c, d, "Pursae SMTP Tools", notices...)
}

func (s *Server) msTestFromDomain(o *msOptions, connID string) string {
	cn := o.conn(connID)
	from := ""
	if cn != nil {
		from = cn.FromEmail
	}
	if from == "" {
		if a := parseAddrList([]string{s.mailer().From}); len(a) == 1 {
			from = a[0].Address
		}
	}
	_, dom, _ := strings.Cut(from, "@")
	return dom
}

// msRunTest sends the Email Test message through one connection, synchronously, with the transcript.
func (s *Server) msRunTest(c *gin.Context, o *msOptions, d *msScreen) {
	r := s.msTest(c.Request.Context(), o, d.TestTo, d.TestConn, d.TestHTML)
	d.TestConn, d.TestResult, d.TestDebug, d.TestHints, d.TestOK = r.Conn, r.Message, r.Debug, r.Hints, r.OK
}

// msTestResult is the outcome of an Email Test (msTest).
type msTestResult struct {
	OK      bool
	Sent    bool     // a delivery was attempted (false: invalid address or Do Not Send)
	Conn    string   // the connection used ("primary" when the asked one does not exist)
	Mailer  string   // its mailer's title
	Message string   // the screen's result line
	Debug   string   // the transport transcript
	Hints   []string // how to fix a failure
}

// msTest sends the Email Test message to one address through one connection (connID, "" or
// "primary" for the primary), synchronously: the Email Log row, the debug event and the last
// error are recorded as for every message. Shared by Tools › Email Test and `pursae mail test`.
func (s *Server) msTest(ctx context.Context, o *msOptions, to, connID string, html bool) msTestResult {
	r := msTestResult{Conn: connID}
	if _, err := parseOneAddr(to); err != nil {
		r.Message = "Please enter a valid email address."
		return r
	}
	if o.General.DoNotSend {
		r.Message = "The \"Do Not Send\" option is enabled under Settings › Misc, so the test email was not sent."
		return r
	}
	cn := o.conn(connID)
	if cn == nil {
		cn, r.Conn = o.primary(), "primary"
	}
	r.Mailer = msMailerTitle(cn.Mailer)
	site := s.settings.String("blogname")
	msg := gomail.Message{To: []string{to}, Subject: "Pursae SMTP: Test Email to " + to,
		Body: "Congrats, test email was sent successfully!\n\nThank you for trying out Pursae SMTP. This test was sent from " + site + " (" + s.settings.String("home") + ") through " + msMailerTitle(cn.Mailer) + ".\n\n- Pursae SMTP"}
	if html {
		msg.HTML = `<div style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;max-width:560px;margin:0 auto;padding:24px;border:1px solid #dcdcde;border-radius:4px">` +
			`<h1 style="font-size:22px;color:#1d2327">Congrats, test email was sent successfully!</h1><p style="color:#3c434a;font-size:15px">Thank you for trying out Pursae SMTP. This test was sent from <strong>` +
			template.HTMLEscapeString(site) + `</strong> through <strong>` + template.HTMLEscapeString(msMailerTitle(cn.Mailer)) + `</strong>.</p><p style="color:#646970">- Pursae SMTP</p></div>`
	}
	var debug bytes.Buffer
	base := s.mailer()
	r.Sent = true
	res := s.msSendVia(ctx, o, cn, base, msg, base.From, &debug)
	s.msLogResult(ctx, o, msg, res, "Pursae SMTP: Email Test")
	r.Debug = debug.String()
	if res.Err != nil {
		s.msSetLastError(ctx, cn.Mailer, res.Err)
		s.msDebugEvent(ctx, o, "error", fmt.Sprintf("Email Test (%s): %v", msMailerTitle(cn.Mailer), res.Err), "Pursae SMTP: Email Test")
		r.Message = res.Err.Error()
		if cn.Mailer == "smtp" {
			// A fixed category, not the raw dial error or the server's banner: the test must not
			// tell an administrator what answers on an arbitrary host and port.
			r.Message = msSMTPErrorMessage(res.Err)
			var se *gomail.SendError
			if !errors.As(res.Err, &se) || se.Stage == gomail.StageConnect {
				r.Debug = ""
			}
		}
		r.Hints = msHints(cn.Mailer, r.Message)
		return r
	}
	s.msClearLastError(ctx)
	r.OK = true
	r.Message = "Test HTML email was sent successfully! Please check your inbox to make sure it was delivered."
	if !html {
		r.Message = "Test plain text email was sent successfully! Please check your inbox to make sure it was delivered."
	}
	return r
}

// msHints are the "how to fix" steps shown under a failed test, like upstream's error explanations.
func msHints(mailer, err string) []string {
	e := strings.ToLower(err)
	var h []string
	switch {
	case strings.Contains(e, "authentication") || strings.Contains(e, "535") || strings.Contains(e, "username and password"):
		h = append(h, "Double-check the SMTP Username and SMTP Password. Some providers (Gmail, Outlook, Yahoo) require an app password.",
			"Make sure the Authentication option is enabled when your server requires it.")
	case strings.Contains(e, "connection refused") || strings.Contains(e, "i/o timeout") || strings.Contains(e, "timed out") || strings.Contains(e, "could not connect") || strings.Contains(e, "no such host"):
		h = append(h, "Check the SMTP Host and SMTP Port. Common ports are 587 (TLS), 465 (SSL) and 25 (none).",
			"Your hosting provider may block outgoing SMTP connections; ask them to open the port, or use an API mailer instead.")
	case strings.Contains(e, "certificate") || strings.Contains(e, "tls") || strings.Contains(e, "starttls"):
		h = append(h, "Try another Encryption setting (TLS on port 587, SSL on port 465), or disable Auto TLS if the server's certificate is broken.")
	case strings.Contains(e, "http 401") || strings.Contains(e, "http 403") || strings.Contains(e, "unauthorized") || strings.Contains(e, "forbidden"):
		h = append(h, "The API key was rejected. Create a new key with sending permission in your "+msMailerTitle(mailer)+" account and paste it again.")
	case strings.Contains(e, "not connected"):
		h = append(h, "Save the Client ID and Client Secret, then click the authorize button in the mailer settings.")
	case strings.Contains(e, "domain") || strings.Contains(e, "sender") || strings.Contains(e, "from"):
		h = append(h, "Verify the From Email address or its domain with "+msMailerTitle(mailer)+".", "Make sure the From Email in Settings › General matches a verified sender.")
	}
	return append(h, "Check the debug output below and the Debug Events (Tools › Debug Events) for details.")
}

// msCheckDomain is the Domain Check of the Email Test: SPF, DMARC and MX records of the From domain.
func msCheckDomain(c *gin.Context, domain, mailer, selector string) []msDNSCheck {
	if domain == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	r := net.DefaultResolver
	var out []msDNSCheck
	spf := msDNSCheck{Name: "SPF", Status: "warning", Detail: "No SPF record found for " + domain + ". Add one that includes your mailer."}
	if txt, err := r.LookupTXT(ctx, domain); err == nil {
		for _, t := range txt {
			if strings.HasPrefix(strings.ToLower(t), "v=spf1") {
				spf.Status, spf.Detail = "ok", t
			}
		}
	}
	dmarc := msDNSCheck{Name: "DMARC", Status: "warning", Detail: "No DMARC record found at _dmarc." + domain + "."}
	if txt, err := r.LookupTXT(ctx, "_dmarc."+domain); err == nil {
		for _, t := range txt {
			if strings.HasPrefix(strings.ToLower(t), "v=dmarc1") {
				dmarc.Status, dmarc.Detail = "ok", t
			}
		}
	}
	mx := msDNSCheck{Name: "MX", Status: "error", Detail: "No MX record found: replies to " + domain + " can't be delivered."}
	if recs, err := r.LookupMX(ctx, domain); err == nil && len(recs) > 0 {
		var hosts []string
		for _, m := range recs {
			hosts = append(hosts, strings.TrimSuffix(m.Host, "."))
		}
		mx.Status, mx.Detail = "ok", strings.Join(hosts, ", ")
	}
	out = append(out, spf, dmarc, mx, msCheckDKIM(ctx, r.LookupTXT, r.LookupCNAME, domain, mailer, selector))
	return out
}

// msDKIMSelectors are the DKIM selectors each mailer's setup guide uses, tried in order.
var msDKIMSelectors = map[string][]string{
	"sendgrid":     {"s1", "s2", "em"},
	"mailgun":      {"mx", "k1", "krs", "smtp", "email"},
	"gmail":        {"google"},
	"outlook":      {"selector1", "selector2"},
	"sendinblue":   {"mail", "brevo1", "brevo2"},
	"mandrill":     {"mandrill"},
	"resend":       {"resend"},
	"mailjet":      {"mailjet"},
	"mailersend":   {"mlsend", "mlsend2"},
	"postmark":     {"pm"},
	"zoho":         {"zoho", "zmail"},
	"sparkpost":    {"scph1119", "sparkpost"},
	"elasticemail": {"api", "ee"},
	"smtp2go":      {"s1"},
}

// msDKIMDefaults are tried after the mailer's own selectors (and for a plain SMTP server).
var msDKIMDefaults = []string{"default", "selector1", "selector2", "google", "k1", "s1", "s2", "mail", "dkim", "smtp"}

// msCheckDKIM looks for a DKIM key under <selector>._domainkey.<domain>: the selector the user
// typed, else the ones the mailer is known to use, else common defaults. Amazon SES publishes
// random-looking CNAME selectors, which cannot be guessed; the user can type theirs.
func msCheckDKIM(ctx context.Context, txt func(context.Context, string) ([]string, error), cname func(context.Context, string) (string, error), domain, mailer, selector string) msDNSCheck {
	var sels []string
	seen := map[string]bool{}
	add := func(list ...string) {
		for _, v := range list {
			if v = strings.ToLower(strings.Trim(strings.TrimSpace(v), ".")); v != "" && !seen[v] {
				seen[v] = true
				sels = append(sels, v)
			}
		}
	}
	if selector = strings.TrimSpace(selector); selector != "" {
		selector = strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(selector), "."+domain), "._domainkey")
		add(selector)
	} else {
		add(msDKIMSelectors[mailer]...)
		add(msDKIMDefaults...)
	}
	for _, sel := range sels {
		name := sel + "._domainkey." + domain
		if recs, err := txt(ctx, name); err == nil {
			for _, t := range recs {
				if l := strings.ToLower(t); strings.Contains(l, "v=dkim1") || strings.Contains(l, "p=") {
					return msDNSCheck{Name: "DKIM", Status: "ok", Detail: "Selector \"" + sel + "\" found at " + name + "."}
				}
			}
		}
		if target, err := cname(ctx, name); err == nil && strings.TrimSuffix(target, ".") != name {
			return msDNSCheck{Name: "DKIM", Status: "ok", Detail: "Selector \"" + sel + "\" found at " + name + " (CNAME to " + strings.TrimSuffix(target, ".") + ")."}
		}
	}
	if selector != "" {
		return msDNSCheck{Name: "DKIM", Status: "warning", Detail: "No DKIM key found at " + selector + "._domainkey." + domain + ". Check the selector and the DNS record your mailer asked you to add."}
	}
	return msDNSCheck{Name: "DKIM", Status: "warning", Detail: "No DKIM key found for the usual " + msMailerTitle(mailer) + " selectors. Your mailer may use another selector: enter it under Domain Check and test again."}
}

// msTestMailer is the mailer slug of a connection ("primary" is the main one).
func (s *Server) msTestMailer(o *msOptions, connID string) string {
	if cn := o.conn(connID); cn != nil {
		return cn.Mailer
	}
	return o.Mail.Mailer
}

// msExport streams the email log as CSV or as a zip of .eml files.
func (s *Server) msExport(c *gin.Context) {
	f := msLogFilter{Status: c.PostForm("status"), From: c.PostForm("date_from"), To: c.PostForm("date_to"), Search: strings.TrimSpace(c.PostForm("s"))}
	logs, _, err := s.msLogList(c.Request.Context(), f, 0, 0)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	stamp := time.Now().In(s.loc()).Format("2006-01-02-150405")
	if c.PostForm("format") == "eml" {
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		for _, e := range logs {
			w, err := z.Create(fmt.Sprintf("email-%d.eml", e.ID))
			if err == nil {
				_, _ = w.Write(msEML(e))
			}
		}
		_ = z.Close()
		c.Header("Content-Disposition", `attachment; filename="wp-mail-smtp-email-logs-`+stamp+`.zip"`)
		c.Data(http.StatusOK, "application/zip", buf.Bytes())
		return
	}
	content := formBool(c, "include_content")
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	head := []string{"ID", "Date", "Status", "Subject", "From", "To", "Reply-To", "Mailer", "Connection", "Source", "Attempts", "Message ID", "Error"}
	if content {
		head = append(head, "Plain Text", "HTML")
	}
	_ = w.Write(head)
	for _, e := range logs {
		row := []string{strconv.FormatInt(e.ID, 10), e.Date.In(s.loc()).Format("2006-01-02 15:04:05"), e.Status, csvSafe(e.Subject), csvSafe(e.From), csvSafe(e.ToStr),
			csvSafe(e.ReplyTo), e.MailerTitle, csvSafe(e.Conn), csvSafe(e.Source), strconv.Itoa(e.Attempts), e.MessageID, csvSafe(e.Error)}
		if content {
			row = append(row, csvSafe(e.Plain), csvSafe(e.HTML))
		}
		_ = w.Write(row)
	}
	w.Flush()
	c.Header("Content-Disposition", `attachment; filename="wp-mail-smtp-email-logs-`+stamp+`.csv"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
}

// csvSafe defuses spreadsheet formulas in exported text.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// --- Setup Wizard ----------------------------------------------------------------------------

func (s *Server) msWizard(c *gin.Context) {
	step := c.DefaultQuery("step", "welcome")
	back := msBase + "wp-mail-smtp-setup-wizard&step="
	o := s.msLoad()
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "wp-mail-smtp-wizard") {
			s.denyNonce(c)
			return
		}
		raw := s.msLoadRaw()
		switch step {
		case "mailer":
			if m := c.PostForm("mailer"); msValidMailer(m) && o.locked["mail.mailer"] == "" {
				raw.Mail.Mailer = m
			}
			_ = s.msSave(c, raw)
			msBack(c, back+"configure")
		case "configure":
			if o.locked["mail.from_email"] == "" {
				raw.Mail.FromEmail = strings.TrimSpace(c.PostForm("from_email"))
			}
			if o.locked["mail.from_name"] == "" {
				raw.Mail.FromName = strings.TrimSpace(c.PostForm("from_name"))
			}
			raw.Mail.FromEmailForce = formBool(c, "from_email_force")
			raw.Mail.FromNameForce = formBool(c, "from_name_force")
			s.msParseMailer(c, o, raw.Mail.Mailer, raw.mailerOpts(raw.Mail.Mailer), true)
			_ = s.msSave(c, raw)
			msBack(c, back+"features")
		case "features":
			raw.Logs.Enabled = formBool(c, "email_log")
			raw.Logs.LogContent = formBool(c, "email_log_content")
			raw.General.SummaryDisabled = !formBool(c, "summary_report_email")
			raw.General.DebugSending = formBool(c, "debug_events")
			if formBool(c, "instant_alert") {
				raw.Alerts.Email.Enabled = true
				if len(raw.Alerts.Email.URLs) == 0 {
					raw.Alerts.Email.URLs = []string{s.settings.String("admin_email")}
				}
			}
			_ = s.msSave(c, raw)
			msBack(c, back+"check")
		case "check":
			d := &msScreen{Page: "wp-mail-smtp-setup-wizard", Step: "check", O: o, Nonce: s.nonce(c, "wp-mail-smtp-wizard"),
				TestTo: strings.TrimSpace(c.PostForm("test_email")), TestHTML: true, TestConn: "primary"}
			s.msRunTest(c, o, d)
			if d.TestOK {
				raw.WizardDone = true
				_ = s.msSave(c, raw)
				d.DomainCheck = msCheckDomain(c, s.msTestFromDomain(o, "primary"), s.msTestMailer(o, "primary"), "")
			}
			s.msRender(c, d, "Pursae SMTP Setup Wizard")
		default:
			msBack(c, back+"welcome")
		}
		return
	}
	d := &msScreen{Page: "wp-mail-smtp-setup-wizard", Step: step, O: o, Nonce: s.nonce(c, "wp-mail-smtp-wizard"), Locked: o.locked,
		TestTo: s.settings.String("admin_email"), RedirectURI: s.msRedirectURI()}
	if d.Locked == nil {
		d.Locked = map[string]string{}
	}
	switch step {
	case "welcome", "features":
	case "mailer":
		d.WizardMailers = s.msMailerViews(c, o, "primary", o.Mail.Mailer, func(m string) map[string]string { return o.Mailers[m] }, true)
	case "configure":
		p := o.primary()
		d.Conn = &msConnView{ID: "primary", Primary: true, FromEmail: p.FromEmail, FromName: p.FromName, Mailer: p.Mailer}
		for _, mv := range s.msMailerViews(c, o, "primary", p.Mailer, func(m string) map[string]string { return o.Mailers[m] }, true) {
			if mv.Selected {
				d.Conn.Mailers = append(d.Conn.Mailers, mv)
			}
		}
	case "check":
	default:
		msBack(c, back+"welcome")
		return
	}
	s.msRender(c, d, "Pursae SMTP Setup Wizard")
}

// --- menu, notices, dashboard widget ---------------------------------------------------------

// mailSMTPMenu adds the "WP Mail SMTP" top-level menu after Settings while the plugin is active.
func (s *Server) mailSMTPMenu(c *gin.Context, menu []*menuTop, screen string) []*menuTop {
	u := currentUser(c)
	if !s.mailSMTPOn() || u == nil || !u.Can("manage_options", nil) {
		return menu
	}
	top := &menuTop{Slug: "admin?page=wp-mail-smtp", Title: "Pursae SMTP", ID: "toplevel_page_wp-mail-smtp", Icon: "dashicons-email-alt"}
	for i, sd := range [][2]string{{"admin?page=wp-mail-smtp", "Settings"}, {"admin?page=wp-mail-smtp-logs", "Email Log"},
		{"admin?page=wp-mail-smtp-reports", "Email Reports"}, {"admin?page=wp-mail-smtp-tools", "Tools"}} {
		ms := &menuSub{Slug: sd[0], Title: template.HTML(sd[1]), First: i == 0, Current: sd[0] == screen}
		top.Current = top.Current || ms.Current
		top.Sub = append(top.Sub, ms)
	}
	top.Current = top.Current || screen == "admin?page=wp-mail-smtp-setup-wizard"
	at := len(menu)
	for i, m := range menu {
		if m.ID == "menu-settings" {
			at = i + 1
		}
	}
	if at > 0 && at <= len(menu) && menu[at-1].Last && !menu[at-1].Separator {
		menu[at-1].Last, top.Last = false, true
	}
	out := make([]*menuTop, 0, len(menu)+1)
	out = append(out, menu[:at]...)
	out = append(out, top)
	return append(out, menu[at:]...)
}

// mailSMTPNotices are the plugin's admin-wide notices: Do Not Send, and the last delivery error.
func (s *Server) mailSMTPNotices(c *gin.Context) []notice {
	u := currentUser(c)
	if !s.mailSMTPOn() || u == nil || !u.Can("manage_options", nil) {
		return nil
	}
	o := s.msLoad()
	var out []notice
	if o.General.DoNotSend {
		out = append(out, notice{Type: "error", Message: template.HTML(`<strong>Pursae SMTP:</strong> The "Do Not Send" option is enabled: this site is not sending any emails. <a href="` +
			msBase + `wp-mail-smtp&amp;tab=misc">Change the setting</a>.`)})
	}
	if st, ok := s.msGetLastError(); ok && !o.General.HideErrors {
		out = append(out, notice{Type: "error", Message: template.HTML(`<strong>Heads up! Pursae SMTP has detected an issue: your last email failed to send.</strong><br>` +
			template.HTMLEscapeString(msMailerTitle(st.Mailer)+": "+st.Err) + `<br><a href="` + msBase + `wp-mail-smtp-logs&amp;status=failed">View Email Log</a> &middot; <a href="` +
			msBase + `wp-mail-smtp">Fix the settings</a> &middot; <a href="` + msBase + `wp-mail-smtp-tools">Send a test email</a>`)})
	}
	if c.Query("error") != "" && strings.HasPrefix(c.Query("page"), msSlug) {
		out = append(out, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(c.Query("error"))), Dismissible: true})
	}
	return out
}

// mailSMTPDashboardWidget renders the "WP Mail SMTP" dashboard widget (empty while inactive or hidden).
func (s *Server) mailSMTPDashboardWidget(c *gin.Context) template.HTML {
	u := currentUser(c)
	if !s.mailSMTPOn() || u == nil || !u.Can("manage_options", nil) {
		return ""
	}
	o := s.msLoad()
	if o.General.HideWidget {
		return ""
	}
	st, err := s.msReport(c.Request.Context(), 7)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	if err := s.tmpl.get(false).ExecuteTemplate(&buf, "mailsmtp-widget", map[string]any{"Stats": st, "Chart": msChart(st), "O": o}); err != nil {
		return template.HTML("<!-- wp-mail-smtp widget: " + template.HTMLEscapeString(err.Error()) + " -->")
	}
	return template.HTML(buf.String())
}

// smtpPrivateHostMessage is the fixed Email Test line when the private-address guard refused the
// SMTP host: the category alone, nothing about what answers there. It is a constant so the
// translation (web/languages) matches it.
const smtpPrivateHostMessage = "SMTP error: the SMTP host is on a private network. Set PURSAE_ALLOW_PRIVATE_ENDPOINTS=1 on the server to allow it."

// msSMTPErrorMessage is the Email Test line for an "Other SMTP" failure (gomail.ErrorCategory);
// a refusal by the private-address guard says how a self-hosted MTA is allowed.
func msSMTPErrorMessage(err error) string {
	if errors.Is(err, netguard.ErrNotPublic) {
		return smtpPrivateHostMessage
	}
	return gomail.ErrorCategory(err)
}
