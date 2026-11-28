package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	gomail "wpgo/internal/mail"
	"wpgo/internal/netguard"
)

// WPGO Mail SMTP is the native clone of WP Mail SMTP (Lite + Pro): it reconfigures wp_mail() to
// send through SMTP or a provider's HTTP API, logs every e-mail, alerts on failures and routes
// messages between connections. Like the other native plugins it starts switched off and adds nothing while
// off: mailer() then hands out the plain env-configured mailer exactly as before.
//
// Settings live in one JSON option, wp_mail_smtp, with WP Mail SMTP's group and key names;
// secrets inside it are AES-GCM encrypted with the server secret (msSeal / msReveal).
// docs/wpgo-mail-smtp-features.md is the feature inventory.

const (
	msSlug    = "wp-mail-smtp"
	msSetting = "native_wp_mail_smtp"
	msOption  = "wp_mail_smtp"
)

// msTestBase points every provider URL (APIs, OAuth, Twilio) at a test server when set.
var msTestBase string

// msHTTP is the client of the API mailers and the alert channels. It refuses private
// addresses at connect time (and on redirects). Tests set msTestBase and may dial loopback.
var msHTTP = publicClient(30*time.Second, func() bool { return msTestBase != "" }, "the address points at a private network")

// msSMTPDialControl guards the admin-set "Other SMTP" host: every connection's resolved address
// must be public, so the mailer cannot probe the server's internal network (DNS rebinding
// included). A self-hosted MTA on loopback or the LAN needs PURSAE_ALLOW_PRIVATE_ENDPOINTS=1;
// PURSAE_SMTP_HOST (the operator's env config, internal/mail Mailer) is not affected.
var msSMTPDialControl = netguard.DialControl(func() bool { return privateEndpointsAllowed() || msTestBase != "" })

// msWebhookURL accepts only http(s) webhook targets. A literal private address is refused
// unless a test has pointed the providers at a local server (msTestBase).
func msWebhookURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return errors.New("webhook URLs must be http or https")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !publicIP(ip) && msTestBase == "" {
		return errors.New("webhook URL points at a private network")
	}
	return nil
}

func (s *Server) mailSMTPOn() bool { return s.settings.Bool(msSetting) }

// msConn is a connection: a mailer with its settings and From address. The primary connection is
// built from the mail group and mailers[mail.mailer]; additional connections are stored whole.
type msConn struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	FromEmail string            `json:"from_email"`
	FromName  string            `json:"from_name"`
	Mailer    string            `json:"mailer"`
	Opts      map[string]string `json:"opts"`

	Title string `json:"-"` // mailer title, for the screens
}

type msRule struct {
	Property string `json:"property"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

type msRoute struct {
	Connection string     `json:"connection"`
	Enabled    bool       `json:"enabled"`
	Groups     [][]msRule `json:"groups"` // OR of AND groups
}

type msHook struct {
	Enabled bool     `json:"enabled"`
	URLs    []string `json:"urls"`
}

type msOptions struct {
	Mail struct {
		FromEmail      string `json:"from_email"`
		FromName       string `json:"from_name"`
		Mailer         string `json:"mailer"`
		ReturnPath     bool   `json:"return_path"`
		FromEmailForce bool   `json:"from_email_force"`
		FromNameForce  bool   `json:"from_name_force"`
	} `json:"mail"`
	Mailers      map[string]map[string]string `json:"mailers"`
	Connections  []msConn                     `json:"connections"`
	Backup       string                       `json:"backup_connection"`
	SmartRouting struct {
		Enabled bool      `json:"enabled"`
		Routes  []msRoute `json:"routes"`
	} `json:"smart_routing"`
	Logs struct {
		Enabled    bool `json:"enabled"`
		LogContent bool `json:"log_email_content"`
		Retention  int  `json:"log_retention_period"` // days, 0 = forever
		// TrackOpens and TrackClicks add a signed pixel / rewrite links of HTML e-mails (off by default;
		// they need the log, which holds the counters). See mailsmtp_track.go.
		TrackOpens  bool `json:"track_opens"`
		TrackClicks bool `json:"track_clicks"`
	} `json:"logs"`
	Alerts struct {
		Threshold int    `json:"failed_threshold"` // alert after N failures in 30 minutes (0/1 = every failure)
		Email     msHook `json:"alert_email"`      // URLs hold the addresses
		Slack     msHook `json:"alert_slack_webhook"`
		Discord   msHook `json:"alert_discord_webhook"`
		Teams     msHook `json:"alert_teams_webhook"`
		Webhook   msHook `json:"alert_custom_webhook"`
		Twilio    struct {
			Enabled bool   `json:"enabled"`
			SID     string `json:"account_sid"`
			Token   string `json:"auth_token"`
			From    string `json:"from_phone_number"`
			To      string `json:"to_phone_number"`
		} `json:"alert_twilio_sms"`
	} `json:"alerts"`
	RateLimit struct {
		Enabled bool `json:"enabled"`
		Minute  int  `json:"minute"`
		Hour    int  `json:"hour"`
		Day     int  `json:"day"`
		Week    int  `json:"week"`
		Month   int  `json:"month"`
	} `json:"rate_limit"`
	Controls map[string]bool `json:"email_controls"` // true: that e-mail is not sent
	General  struct {
		DoNotSend         bool `json:"do_not_send"`
		HideAnnouncements bool `json:"am_notifications_hidden"`
		HideErrors        bool `json:"email_delivery_errors_hidden"`
		HideWidget        bool `json:"dashboard_widget_hidden"`
		SummaryDisabled   bool `json:"summary_report_email_disabled"`
		Uninstall         bool `json:"uninstall"`
		DebugSending      bool `json:"debug_email_sending"`
		DebugRetention    int  `json:"debug_events_retention_period"` // days, 0 = forever
	} `json:"general"`
	WizardDone bool `json:"wizard_done"`

	locked map[string]string // "group.key" -> constant name, for fields set from the environment
}

// msLoad reads the plugin options with the WPMS_* environment overrides applied.
func (s *Server) msLoad() *msOptions {
	o := s.msLoadRaw()
	o.applyEnv()
	return o
}

// msLoadRaw reads the stored options only: the base for changing and saving them.
func (s *Server) msLoadRaw() *msOptions {
	o := &msOptions{}
	o.Mail.Mailer = "mail"
	o.Mail.ReturnPath = true
	o.Logs.Enabled = true
	o.Logs.Retention = 0
	if raw, err := json.Marshal(s.settings.Get(msOption)); err == nil && string(raw) != "null" {
		_ = json.Unmarshal(raw, o)
	}
	if o.Mailers == nil {
		o.Mailers = map[string]map[string]string{}
	}
	if o.Controls == nil {
		o.Controls = map[string]bool{}
	}
	return o
}

// msSave stores options read with msLoadRaw.
func (s *Server) msSave(ctx context.Context, o *msOptions) error {
	clean := *o
	clean.locked = nil
	return s.settings.Set(ctx, map[string]any{msOption: clean})
}

func (o *msOptions) mailerOpts(mailer string) map[string]string {
	m := o.Mailers[mailer]
	if m == nil {
		m = map[string]string{}
		o.Mailers[mailer] = m
	}
	return m
}

func (o *msOptions) primary() *msConn {
	opts := map[string]string{}
	for k, v := range o.Mailers[o.Mail.Mailer] {
		opts[k] = v
	}
	return &msConn{ID: "primary", Name: "Primary Connection", FromEmail: o.Mail.FromEmail, FromName: o.Mail.FromName, Mailer: o.Mail.Mailer, Opts: opts}
}

// conn finds a connection by id ("primary" or an additional connection).
func (o *msOptions) conn(id string) *msConn {
	if id == "primary" || id == "" {
		return o.primary()
	}
	for i := range o.Connections {
		if o.Connections[i].ID == id {
			return &o.Connections[i]
		}
	}
	return nil
}

// --- environment constants (wp-config.php's WPMS_* defines) ---------------------------------

// msConstName is upstream's constant for a mailer setting.
func msConstName(mailer, key string) string {
	if mailer == "smtp" {
		switch key {
		case "encryption":
			return "WPMS_SSL"
		case "auth", "autotls", "host", "port", "user", "pass":
			return "WPMS_SMTP_" + strings.ToUpper(key)
		}
	}
	return "WPMS_" + strings.ToUpper(mailer) + "_" + strings.ToUpper(key)
}

func envBool(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func (o *msOptions) applyEnv() {
	if !envBool(os.Getenv("WPMS_ON")) {
		return
	}
	o.locked = map[string]string{}
	str := func(name, field string, dst *string) {
		if v, ok := os.LookupEnv(name); ok {
			*dst = v
			o.locked[field] = name
		}
	}
	boolean := func(name, field string, dst *bool) {
		if v, ok := os.LookupEnv(name); ok {
			*dst = envBool(v)
			o.locked[field] = name
		}
	}
	integer := func(name, field string, dst *int) {
		if v, ok := os.LookupEnv(name); ok {
			*dst, _ = strconv.Atoi(strings.TrimSpace(v))
			o.locked[field] = name
		}
	}
	list := func(name, field string, h *msHook) {
		if v, ok := os.LookupEnv(name); ok {
			h.Enabled, h.URLs = true, splitList(v)
			o.locked[field] = name
		}
	}
	str("WPMS_MAILER", "mail.mailer", &o.Mail.Mailer)
	str("WPMS_MAIL_FROM", "mail.from_email", &o.Mail.FromEmail)
	str("WPMS_MAIL_FROM_NAME", "mail.from_name", &o.Mail.FromName)
	boolean("WPMS_MAIL_FROM_FORCE", "mail.from_email_force", &o.Mail.FromEmailForce)
	boolean("WPMS_MAIL_FROM_NAME_FORCE", "mail.from_name_force", &o.Mail.FromNameForce)
	boolean("WPMS_SET_RETURN_PATH", "mail.return_path", &o.Mail.ReturnPath)
	boolean("WPMS_DO_NOT_SEND", "general.do_not_send", &o.General.DoNotSend)
	boolean("WPMS_SUMMARY_REPORT_EMAIL_DISABLED", "general.summary_report_email_disabled", &o.General.SummaryDisabled)
	integer("WPMS_DEBUG_EVENTS_RETENTION_PERIOD", "general.debug_events_retention_period", &o.General.DebugRetention)
	boolean("WPMS_LOGS_ENABLED", "logs.enabled", &o.Logs.Enabled)
	boolean("WPMS_LOGS_LOG_EMAIL_CONTENT", "logs.log_email_content", &o.Logs.LogContent)
	if v, ok := os.LookupEnv("WPMS_LOGS_LOG_RETENTION_PERIOD"); ok {
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		o.Logs.Retention = n / 86400 // upstream counts seconds
		o.locked["logs.log_retention_period"] = "WPMS_LOGS_LOG_RETENTION_PERIOD"
	}
	list("WPMS_ALERT_EMAIL_SEND_TO", "alerts.alert_email", &o.Alerts.Email)
	list("WPMS_ALERT_SLACK_WEBHOOK_URL", "alerts.alert_slack_webhook", &o.Alerts.Slack)
	list("WPMS_ALERT_DISCORD_WEBHOOK_URL", "alerts.alert_discord_webhook", &o.Alerts.Discord)
	list("WPMS_ALERT_TEAMS_WEBHOOK_URL", "alerts.alert_teams_webhook", &o.Alerts.Teams)
	list("WPMS_ALERT_CUSTOM_WEBHOOK_URL", "alerts.alert_custom_webhook", &o.Alerts.Webhook)
	if v, ok := os.LookupEnv("WPMS_ALERT_TWILIO_SMS_ACCOUNT_SID"); ok {
		t := &o.Alerts.Twilio
		t.Enabled, t.SID = true, v
		t.Token = os.Getenv("WPMS_ALERT_TWILIO_SMS_AUTH_TOKEN")
		t.From = os.Getenv("WPMS_ALERT_TWILIO_SMS_FROM_PHONE_NUMBER")
		t.To = os.Getenv("WPMS_ALERT_TWILIO_SMS_TO_PHONE_NUMBER")
		o.locked["alerts.alert_twilio_sms"] = "WPMS_ALERT_TWILIO_SMS_*"
	}
	for _, d := range msMailers {
		for _, f := range d.Fields {
			name := msConstName(d.Slug, f.Key)
			if v, ok := os.LookupEnv(name); ok {
				if f.Type == "checkbox" {
					v = map[bool]string{true: "1", false: ""}[envBool(v)]
				}
				o.mailerOpts(d.Slug)[f.Key] = v
				o.locked[d.Slug+"."+f.Key] = name
			}
		}
	}
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// --- secrets ---------------------------------------------------------------------------------

const msSealPrefix = "enc:v1:"

func (s *Server) msKey() []byte {
	secret := s.cfg.Secret
	if secret == "" {
		secret = s.settings.String("auth_secret")
	}
	k := sha256.Sum256([]byte("wpgo-mail-smtp|" + secret))
	return k[:]
}

// msSeal encrypts a secret for storage; empty stays empty.
func (s *Server) msSeal(plain string) string {
	if plain == "" || strings.HasPrefix(plain, msSealPrefix) {
		return plain
	}
	block, _ := aes.NewCipher(s.msKey())
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return msSealPrefix + base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil))
}

// msReveal decrypts a stored secret; values from the environment are plain and pass through.
func (s *Server) msReveal(v string) string {
	raw, ok := strings.CutPrefix(v, msSealPrefix)
	if !ok {
		return v
	}
	data, err := base64.RawStdEncoding.DecodeString(raw)
	block, _ := aes.NewCipher(s.msKey())
	gcm, _ := cipher.NewGCM(block)
	if err != nil || len(data) < gcm.NonceSize() {
		return ""
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		slog.Warn("wp-mail-smtp: a stored secret cannot be decrypted (was the server secret changed?)")
		return ""
	}
	return string(plain)
}

// msSecretKeys are the mailer settings stored encrypted and never printed into a form.
var msSecretKeys = map[string]bool{"pass": true, "api_key": true, "client_secret": true, "secret_key": true, "server_api_token": true,
	"access_token": true, "refresh_token": true}

// revealed returns a connection's settings in plain text for a send.
func (s *Server) msRevealed(opts map[string]string) map[string]string {
	out := make(map[string]string, len(opts))
	for k, v := range opts {
		out[k] = s.msReveal(v)
	}
	return out
}

// --- delivery --------------------------------------------------------------------------------

// mailSMTPAttach is called by mailer(): while the plugin is active, wp_mail() goes through it.
func (s *Server) mailSMTPAttach(m *gomail.Mailer) {
	if s.mailSMTPOn() {
		m.Transport = &msTransport{s: s, defaultFrom: m.From}
	}
}

type msTransport struct {
	s           *Server
	defaultFrom string // the site's From before a sender (a form) set its own
}

func (t *msTransport) Deliver(ctx context.Context, m *gomail.Mailer, msg gomail.Message) error {
	return t.s.msDeliver(ctx, m, msg, t.defaultFrom)
}

// msResult is one delivery attempt.
type msResult struct {
	MessageID, From, Envelope string
	TrackID                   string // tracking id of the logged message, "" when not tracked
	Conn                      *msConn
	Err                       error
}

// msEmailControls are WordPress Go's core notifications Email Controls can switch off, matched by
// the subject after the "[Site] " prefix.
var msEmailControls = []struct {
	Key, Group, Label, Prefix string
}{
	{"comment_moderation", "Comments", "Awaiting Moderation", "Please moderate: "},
	{"comment_published", "Comments", "Published (to the post author)", "Comment: "},
	{"admin_email_change", "Change of Admin Email", "Site Admin Email Change Attempt", "New Admin Email Address"},
	{"user_email_change", "Change of User Email or Password", "User Email Change Request", "Email Change Request"},
	{"password_reset", "Change of User Email or Password", "Reset Password Request", "Password Reset"},
	{"password_changed", "Change of User Email or Password", "Password Changed (to the admin)", "Password Changed"},
	{"new_user_admin", "Personal Data Requests / Users", "New User Created (to the admin)", "New User Registration"},
	{"new_user_user", "Personal Data Requests / Users", "New User Created (login details to the user)", "Login Details"},
	{"privacy_confirm", "Personal Data Requests / Users", "User Confirms Personal Data Request", "Confirm Action: "},
	{"privacy_confirmed", "Personal Data Requests / Users", "Personal Data Request Confirmed (to the admin)", "Action Confirmed: "},
	{"privacy_export", "Personal Data Requests / Users", "Personal Data Export Ready", "Personal Data Export"},
	{"privacy_erased", "Personal Data Requests / Users", "Personal Data Erased", "Erasure Request Fulfilled"},
}

func init() {
	// Pursae Commerce e-mails carry Message.Kind = "goo_<email id>" and have no subject prefix.
	for _, e := range [][2]string{{"new_order", "New order"}, {"cancelled_order", "Cancelled order"}, {"failed_order", "Failed order"},
		{"customer_on_hold_order", "Order on-hold"}, {"customer_processing_order", "Processing order"}, {"customer_completed_order", "Completed order"},
		{"customer_refunded_order", "Refunded order"}, {"customer_invoice", "Order details"}, {"customer_note", "Customer note"},
		{"customer_reset_password", "Reset password"}, {"customer_new_account", "New account"}, {"low_stock", "Low stock"},
		{"no_stock", "Out of stock"}, {"backorder", "Backorder"}} {
		msEmailControls = append(msEmailControls, struct{ Key, Group, Label, Prefix string }{"goo_" + e[0], "Pursae Commerce", e[1], ""})
	}
}

// msClassifyMsg classifies a message by its Kind (set by the sender) and falls back to the subject
// prefix for mail that carries none (queued before the field existed, or sent by plugins).
func msClassifyMsg(msg gomail.Message) (key, label string) {
	if msg.Kind != "" {
		for _, c := range msEmailControls {
			if c.Key == msg.Kind {
				return c.Key, c.Label
			}
		}
	}
	return msClassify(msg.Subject)
}

// msClassify names the core notification a subject belongs to ("" for other e-mail) and its label.
func msClassify(subject string) (key, label string) {
	if !strings.HasPrefix(subject, "[") {
		return "", ""
	}
	// The site title may itself contain "] ", so try every place the prefix could end.
	for i := strings.Index(subject, "] "); i > 0; {
		rest := subject[i+2:]
		for _, c := range msEmailControls {
			if c.Prefix != "" && strings.HasPrefix(rest, c.Prefix) {
				return c.Key, c.Label
			}
		}
		j := strings.Index(rest, "] ")
		if j < 0 {
			break
		}
		i += 2 + j
	}
	return "", ""
}

// msContactFormInitiator is the log initiator of a contact form mail that answers to the visitor
// (privacy requests match these rows by their Reply-To).
const msContactFormInitiator = "Pursae Contact Form"

func (s *Server) msDeliver(ctx context.Context, base *gomail.Mailer, msg gomail.Message, defaultFrom string) error {
	o := s.msLoad()
	ctrl, label := msClassifyMsg(msg)
	source := "Pursae"
	if msg.Kind == gontactVisitorReplyKind {
		source = msContactFormInitiator
	} else if label != "" {
		source = "Pursae: " + label
	} else if msOwnFrom(base, msg, defaultFrom) != "" {
		source = "Form"
	}
	if ctrl != "" && o.Controls[ctrl] {
		return nil // switched off under Email Controls
	}
	if o.General.DoNotSend {
		s.msDebugEvent(ctx, o, "debug", fmt.Sprintf("Do Not Send is on: the e-mail %q to %s was not sent.", msg.Subject, strings.Join(msg.To, ", ")), source)
		return nil
	}
	conn := o.primary()
	if o.SmartRouting.Enabled {
		if c := o.route(base, msg, source, defaultFrom); c != nil {
			conn = c
		}
	}
	slot, next, limited := s.msReserveRate(ctx, o)
	if limited {
		s.msDebugEvent(ctx, o, "debug", fmt.Sprintf("Rate limit reached: the e-mail %q was queued until %s.", msg.Subject, next.In(s.loc()).Format("Jan 2, 15:04")), source)
		return s.queueJob(ctx, "wp_mail", msg, next)
	}
	var debug strings.Builder
	var dbg io.Writer
	if o.General.DebugSending {
		dbg = &debug
	}
	sendMsg, trackID := s.msTrackMessage(ctx, o, msg)
	res := s.msSendVia(ctx, o, conn, base, sendMsg, defaultFrom, dbg)
	if res.Err != nil && o.Backup != "" && o.Backup != conn.ID {
		if b := o.conn(o.Backup); b != nil {
			first := res.Err
			s.msDebugEvent(ctx, o, "error", fmt.Sprintf("%s failed (%v); retrying with the backup connection %q.", msMailerTitle(conn.Mailer), first, b.Name), source)
			res = s.msSendVia(ctx, o, b, base, sendMsg, defaultFrom, dbg)
			if res.Err != nil {
				res.Err = fmt.Errorf("%v; backup connection: %w", first, res.Err)
			}
		}
	}
	if res.Err != nil {
		s.msReleaseRate(ctx, slot) // a failed delivery does not use up the quota
	}
	res.TrackID = trackID
	firstFailure := s.msLogResult(ctx, o, msg, res, source)
	if res.Err != nil {
		s.msSetLastError(ctx, res.Conn.Mailer, res.Err)
		s.msDebugEvent(ctx, o, "error", fmt.Sprintf("Mailer: %s\n%v", msMailerTitle(res.Conn.Mailer), res.Err), source)
		if firstFailure {
			s.msAlert(ctx, o, msg, res)
		}
		return res.Err
	}
	s.msClearLastError(ctx)
	if o.General.DebugSending && debug.Len() > 0 {
		s.msDebugEvent(ctx, o, "debug", debug.String(), source)
	}
	return nil
}

// msErrorState is the last delivery failure, shown as an admin notice until an e-mail goes out.
type msErrorState struct {
	Err, Mailer string
	At          time.Time
}

const msLastErrorKey = "wp_mail_smtp_last_error"

// msSetLastError stores the last delivery failure in the settings table, so it survives a restart
// and shows on every instance.
func (s *Server) msSetLastError(ctx context.Context, mailer string, err error) {
	b, _ := json.Marshal(msErrorState{Err: err.Error(), At: time.Now(), Mailer: mailer})
	_ = s.settings.Set(context.WithoutCancel(ctx), map[string]any{msLastErrorKey: string(b)})
}

// msClearLastError forgets the last failure (an e-mail went out, or the plugin was deactivated).
func (s *Server) msClearLastError(ctx context.Context) {
	if s.settings.String(msLastErrorKey) == "" {
		return
	}
	_ = s.settings.Set(context.WithoutCancel(ctx), map[string]any{msLastErrorKey: ""})
}

// msGetLastError returns the stored last delivery failure.
func (s *Server) msGetLastError() (msErrorState, bool) {
	var st msErrorState
	raw := s.settings.String(msLastErrorKey)
	if raw == "" || json.Unmarshal([]byte(raw), &st) != nil || st.Err == "" {
		return st, false
	}
	return st, true
}

// msOwnFrom is the From a sender chose for one message (a form's "From" field travels as
// Message.From through the mail queue; older callers set it on the Mailer), "" when none.
func msOwnFrom(base *gomail.Mailer, msg gomail.Message, defaultFrom string) string {
	if msg.From != "" {
		if _, err := mail.ParseAddress(msg.From); err == nil {
			return msg.From
		}
	}
	if base.From != defaultFrom {
		return base.From
	}
	return ""
}

// msFrom works out the From of a message on a connection, honouring Force From Email / Name.
func (s *Server) msFrom(o *msOptions, conn *msConn, base *gomail.Mailer, msg gomail.Message, defaultFrom string) (from *mail.Address, envelope string) {
	def, _ := mail.ParseAddress(defaultFrom)
	if def == nil {
		def = &mail.Address{Name: s.settings.String("blogname"), Address: "wordpress@localhost"}
	}
	from = &mail.Address{Name: conn.FromName, Address: conn.FromEmail}
	if from.Address == "" {
		from.Address = def.Address
	}
	if from.Name == "" {
		from.Name = def.Name
	}
	envelope = from.Address
	if ownFrom := msOwnFrom(base, msg, defaultFrom); ownFrom != "" { // the sender chose its own From (a form's "From Email")
		if own, err := mail.ParseAddress(ownFrom); err == nil {
			if !o.Mail.FromEmailForce {
				from.Address = own.Address
				if o.Mail.ReturnPath {
					envelope = own.Address
				}
			}
			if !o.Mail.FromNameForce && own.Name != "" {
				from.Name = own.Name
			}
		}
	}
	return from, envelope
}

// msSendVia sends one message through one connection.
func (s *Server) msSendVia(ctx context.Context, o *msOptions, conn *msConn, base *gomail.Mailer, msg gomail.Message, defaultFrom string, debug io.Writer) msResult {
	from, envelope := s.msFrom(o, conn, base, msg, defaultFrom)
	res := msResult{From: from.String(), Envelope: envelope, Conn: conn}
	core := *base
	core.Transport = nil
	core.From = from.String()
	msg.From = "" // the header shows the From worked out above (Force From Email applies to it)
	opts := s.msRevealed(conn.Opts)
	if debug != nil {
		fmt.Fprintf(debug, "Mailer: %s\nFrom: %s\n", msMailerTitle(conn.Mailer), from.String())
	}
	switch conn.Mailer {
	case "", "mail":
		res.Err = core.Send(ctx, msg)
		return res
	case "smtp":
		raw, err := core.Build(msg)
		if err != nil {
			res.Err = err
			return res
		}
		var rcpt []string
		for _, a := range parseAddrList(append(append(append([]string{}, msg.To...), msg.Cc...), msg.Bcc...)) { // like mail.go
			rcpt = append(rcpt, a.Address)
		}
		c := &gomail.SMTP{Host: opts["host"], Port: opts["port"], Encryption: opts["encryption"], AutoTLS: opts["autotls"] != "",
			Auth: opts["auth"] != "", User: opts["user"], Pass: opts["pass"], LocalName: hostOf(s.settings.String("home")), Debug: debug,
			Control: msSMTPDialControl}
		res.MessageID, res.Err = c.Send(ctx, envelope, rcpt, raw)
		return res
	}
	if msOAuthMailer(conn.Mailer) {
		tok, err := s.msAccessToken(ctx, conn)
		if err != nil {
			res.Err = err
			return res
		}
		opts["access_token"] = tok
	}
	raw, err := core.Build(msg)
	if err != nil {
		res.Err = err
		return res
	}
	api := &gomail.API{Mailer: conn.Mailer, Opts: opts, Client: msHTTP, Base: msTestBase, Debug: debug}
	res.MessageID, res.Err = api.Send(ctx, gomail.Email{FromName: from.Name, FromEmail: from.Address, To: msg.To, Cc: msg.Cc, Bcc: msg.Bcc,
		ReplyTo: msg.ReplyTo, Subject: msg.Subject, Text: msg.Body, HTML: msg.HTML, Attachments: msg.Attachments, Raw: raw})
	return res
}

func parseAddrList(list []string) []*mail.Address {
	var out []*mail.Address
	for _, v := range list {
		if a, err := mail.ParseAddress(v); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// --- OAuth tokens ----------------------------------------------------------------------------

func msOAuthMailer(m string) bool { return m == "gmail" || m == "outlook" || m == "zoho" }

func (s *Server) msOAuth(conn *msConn) *gomail.OAuth {
	opts := s.msRevealed(conn.Opts)
	return &gomail.OAuth{Mailer: conn.Mailer, Domain: opts["domain"], ClientID: opts["client_id"], ClientSecret: opts["client_secret"],
		RedirectURI: s.msRedirectURI(), Client: msHTTP, Base: msTestBase}
}

func (s *Server) msRedirectURI() string {
	return strings.TrimRight(s.settings.String("home"), "/") + "/wp-admin/admin?page=" + msSlug + "&tab=auth"
}

// msAccessToken returns a valid access token for an OAuth connection, refreshing (and storing) it
// when it is about to expire.
func (s *Server) msAccessToken(ctx context.Context, conn *msConn) (string, error) {
	opts := s.msRevealed(conn.Opts)
	if opts["refresh_token"] == "" {
		return "", fmt.Errorf("%s is not connected: click \"Allow plugin to send emails using your %s account\" in the mailer settings", msMailerTitle(conn.Mailer), msMailerTitle(conn.Mailer))
	}
	exp, _ := strconv.ParseInt(opts["expires"], 10, 64)
	if opts["access_token"] != "" && time.Now().Add(time.Minute).Unix() < exp {
		return opts["access_token"], nil
	}
	tok, err := s.msOAuth(conn).Refresh(ctx, opts["refresh_token"])
	if err != nil {
		return "", err
	}
	s.msStoreToken(ctx, conn.ID, conn.Mailer, tok, "", "")
	return tok.AccessToken, nil
}

// msStoreToken saves an OAuth token pair (and the account) on a connection.
func (s *Server) msStoreToken(ctx context.Context, connID, mailer string, tok *gomail.Token, user, account string) {
	o := s.msLoadRaw()
	var opts map[string]string
	if connID == "primary" {
		opts = o.mailerOpts(mailer)
	} else if c := o.conn(connID); c != nil {
		if c.Opts == nil {
			c.Opts = map[string]string{}
		}
		opts = c.Opts
	}
	if opts == nil {
		return
	}
	opts["access_token"] = s.msSeal(tok.AccessToken)
	opts["refresh_token"] = s.msSeal(tok.RefreshToken)
	opts["expires"] = strconv.FormatInt(tok.Expiry.Unix(), 10)
	if user != "" {
		opts["oauth_user"] = user
	}
	if account != "" {
		opts["account_id"] = account
	}
	if err := s.msSave(ctx, o); err != nil {
		slog.Error("wp-mail-smtp: saving OAuth token", "err", err)
	}
}

// --- smart routing ---------------------------------------------------------------------------

// route returns the connection of the first enabled route whose conditions match.
func (o *msOptions) route(base *gomail.Mailer, msg gomail.Message, source, defaultFrom string) *msConn {
	own := msOwnFrom(base, msg, defaultFrom)
	if own == "" {
		own = base.From
	}
	from, _ := mail.ParseAddress(own)
	if from == nil {
		from = &mail.Address{}
	}
	props := map[string]string{"subject": msg.Subject, "message": msg.Body + "\n" + msg.HTML, "from_email": from.Address, "from_name": from.Name,
		"to": strings.Join(msg.To, ", "), "reply_to": msg.ReplyTo, "initiator": source}
	for _, r := range o.SmartRouting.Routes {
		if !r.Enabled {
			continue
		}
		for _, g := range r.Groups {
			if len(g) == 0 {
				continue
			}
			all := true
			for _, rule := range g {
				if !msRuleMatch(rule, props[rule.Property]) {
					all = false
					break
				}
			}
			if all {
				if c := o.conn(r.Connection); c != nil {
					return c
				}
			}
		}
	}
	return nil
}

func msRuleMatch(r msRule, v string) bool {
	lv, lw := strings.ToLower(v), strings.ToLower(r.Value)
	switch r.Operator {
	case "is":
		return lv == lw
	case "is_not":
		return lv != lw
	case "contains":
		return strings.Contains(lv, lw)
	case "not_contains":
		return !strings.Contains(lv, lw)
	case "starts":
		return strings.HasPrefix(lv, lw)
	case "ends":
		return strings.HasSuffix(lv, lw)
	case "regex", "not_regex":
		re, err := regexp.Compile(r.Value)
		if err != nil {
			return false
		}
		return re.MatchString(v) == (r.Operator == "regex")
	}
	return false
}

// --- rate limiting ---------------------------------------------------------------------------

// msRateLock serialises the count-and-reserve step of msReserveRate across instances.
const msRateLock = 7301954

// msReserveRate checks the limits and, when there is room, records the send slot in the same
// transaction (under an advisory lock), so concurrent deliveries cannot all see the same count and
// overshoot. It returns the reserved slot (zero when no limit is on), or when the next slot opens.
func (s *Server) msReserveRate(ctx context.Context, o *msOptions) (slot, next time.Time, limited bool) {
	if !o.RateLimit.Enabled {
		return
	}
	windows := []struct {
		limit int
		d     time.Duration
	}{{o.RateLimit.Minute, time.Minute}, {o.RateLimit.Hour, time.Hour}, {o.RateLimit.Day, 24 * time.Hour},
		{o.RateLimit.Week, 7 * 24 * time.Hour}, {o.RateLimit.Month, 30 * 24 * time.Hour}}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(msRateLock)); err != nil {
		return
	}
	for _, w := range windows {
		if w.limit <= 0 {
			continue
		}
		var oldest *time.Time
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*), min(sent_at) FROM (SELECT sent_at FROM wpmailsmtp_rate
			WHERE sent_at > now() - $1::interval ORDER BY sent_at DESC LIMIT $2) x`, fmt.Sprintf("%d seconds", int(w.d.Seconds())), w.limit).Scan(&n, &oldest)
		if err != nil || n < w.limit || oldest == nil {
			continue
		}
		if t := oldest.Add(w.d); t.After(next) {
			next = t
		}
	}
	if !next.IsZero() {
		return time.Time{}, next, true
	}
	if err := tx.QueryRow(ctx, `INSERT INTO wpmailsmtp_rate(sent_at) VALUES (clock_timestamp()) RETURNING sent_at`).Scan(&slot); err != nil {
		return time.Time{}, time.Time{}, false
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, time.Time{}, false
	}
	return slot, time.Time{}, false
}

// msReleaseRate gives a reserved slot back (the delivery failed).
func (s *Server) msReleaseRate(ctx context.Context, slot time.Time) {
	if slot.IsZero() {
		return
	}
	_, _ = s.store.Pool.Exec(context.WithoutCancel(ctx), `DELETE FROM wpmailsmtp_rate WHERE ctid = (SELECT ctid FROM wpmailsmtp_rate WHERE sent_at = $1 LIMIT 1)`, slot)
}

// --- activation ------------------------------------------------------------------------------

func (s *Server) mailSMTPActivate(ctx context.Context) error {
	if err := s.settings.Set(ctx, map[string]any{msSetting: true}); err != nil {
		return err
	}
	_, err := s.store.Pool.Exec(ctx, `INSERT INTO jobs(name, run_at, recurrence)
		SELECT 'wp_mail_smtp_daily', now() + interval '5 minutes', 'daily' WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE name = 'wp_mail_smtp_daily' AND recurrence <> '')`)
	return err
}

func (s *Server) mailSMTPDeactivate(ctx context.Context) error {
	o := s.msLoad()
	if err := s.settings.Set(ctx, map[string]any{msSetting: false}); err != nil {
		return err
	}
	s.msClearLastError(ctx)
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM jobs WHERE name = 'wp_mail_smtp_daily'`)
	if o.General.Uninstall { // Misc › Uninstall WP Mail SMTP: remove every trace
		for _, q := range []string{`DELETE FROM wpmailsmtp_emails_log`, `DELETE FROM wpmailsmtp_debug_events`, `DELETE FROM wpmailsmtp_rate`,
			`DELETE FROM settings WHERE key IN ('wp_mail_smtp', 'wp_mail_smtp_summary_sent', 'wp_mail_smtp_last_error', 'wp_mail_smtp_last_alert')`} {
			if _, err := s.store.Pool.Exec(ctx, q); err != nil {
				return err
			}
		}
		return s.settings.Load(ctx)
	}
	return nil
}
