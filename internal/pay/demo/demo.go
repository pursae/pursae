//go:build paydemo

// Package demo is a development-only payment provider: a fake bank whose hosted page runs inside this
// process, so the whole hosted flow (checkout, the bridge's payment page, the provider's page, the
// signed callback, the paid order) can be tried locally without a merchant account.
//
// It exists only in binaries built with the tag paydemo; release builds never set it. Its page is
// served on http://127.0.0.1 (a random free port) because it is loopback and development only. It has
// no card fields, not even fake ones: the page offers Approve, Decline and Cancel.
package demo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"wpgo/internal/pay"
)

// Origin is the host the demo page is served on, without its port (the bridge's loopback allowance
// matches any port of it).
const Origin = "http://127.0.0.1"

// Spec describes the demo provider.
func Spec() pay.Spec {
	return pay.Spec{
		ID:    "demo",
		Title: "Demo bank (development)",
		Description: "A fake bank for trying the hosted payment flow locally. It only exists in " +
			"development builds (build tag paydemo) and never charges anything.",
		Region:   pay.RegionTR,
		Kind:     pay.KindBank,
		Features: []pay.Feature{pay.Feature3DS, pay.FeatureInstallments, pay.FeatureRefunds, pay.FeatureQuery},
		Fields: []pay.Field{
			{ID: "secret", Title: "Signing secret", Type: "password", Secret: true, Required: true,
				Desc: "Any text: the demo page signs its answers with it and the store checks the signature."},
			{ID: "flow", Title: "Flow", Type: "select", Default: "form",
				Desc: "How the store sends the customer to the demo page.",
				Options: []pay.Option{{Value: "form", Label: "Auto-posted form (bank 3-D Secure style)"},
					{Value: "embed", Label: "Iframe on the store's payment page"}, {Value: "redirect", Label: "Redirect"}}},
		},
		Hosts: pay.Hosts{Frame: []string{Origin}, Form: []string{Origin}},
		New:   newProvider,
	}
}

type provider struct {
	secret string
	flow   string
}

func newProvider(c pay.Config) (pay.Provider, error) {
	if err := c.Require("secret"); err != nil {
		return nil, err
	}
	flow := c.Value("flow")
	switch flow {
	case "form", "embed", "redirect":
	case "":
		flow = "form"
	default:
		return nil, errors.New("demo: unknown flow")
	}
	return &provider{secret: c.Value("secret"), flow: flow}, nil
}

// Sign is the signature of a demo answer: HMAC-SHA256 hex of "ref|status|amount|currency".
func Sign(secret, ref, status string, amount int64, currency string) string {
	msg := ref + "|" + status + "|" + strconv.FormatInt(amount, 10) + "|" + currency
	return hex.EncodeToString(pay.HMACSHA256([]byte(secret), []byte(msg)))
}

// session is a started payment as the demo page knows it.
type session struct {
	ref, currency, desc string
	amount              int64
	installment         int
	returnURL, failURL  string
	secret              string
	embed               bool
}

// The demo "bank": one loopback listener per process, started on the first payment.
var bank struct {
	once     sync.Once
	err      error
	base     string
	mu       sync.Mutex
	sessions map[string]*session // by token
	approved map[string]pay.Result
	refunds  map[string][]int64
}

// base starts the listener if needed and returns its origin, "http://127.0.0.1:<port>".
func base() (string, error) {
	bank.once.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			bank.err = err
			return
		}
		bank.sessions = map[string]*session{}
		bank.approved = map[string]pay.Result{}
		bank.refunds = map[string][]int64{}
		bank.base = "http://" + ln.Addr().String()
		mux := http.NewServeMux()
		mux.HandleFunc("/pay", servePage)
		mux.HandleFunc("/decide", serveDecide)
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(ln)
	})
	return bank.base, bank.err
}

func token() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	b, err := base()
	if err != nil {
		return nil, pay.Errorf(pay.CodeProviderDown, "", "demo listener: %v", err)
	}
	tok := token()
	bank.mu.Lock()
	bank.sessions[tok] = &session{ref: r.Ref, currency: r.Currency, desc: r.Description, amount: r.Amount, installment: r.Installment,
		returnURL: r.ReturnURL, failURL: defaultStr(r.FailURL, r.ReturnURL), secret: p.secret, embed: p.flow == "embed"}
	bank.mu.Unlock()
	page := b + "/pay"
	switch p.flow {
	case "embed":
		return &pay.StartResult{Embed: &pay.Embed{FrameURL: page + "?t=" + tok}, ProviderRef: tok}, nil
	case "redirect":
		return &pay.StartResult{Redirect: page + "?t=" + tok, ProviderRef: tok}, nil
	}
	return &pay.StartResult{Form: &pay.Form{Action: page, Fields: [][2]string{{"t", tok}}}, ProviderRef: tok}, nil
}

func defaultStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (p *provider) CallbackRef(in pay.CallbackInput) string {
	if v := in.Form.Get("ref"); v != "" {
		return v
	}
	return in.Query.Get("ref")
}

func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	f := in.Form
	ref, status, cur := f.Get("ref"), f.Get("status"), f.Get("currency")
	amount, err := strconv.ParseInt(f.Get("amount"), 10, 64)
	if err != nil {
		return nil, pay.ErrInvalidSignature
	}
	if !pay.Equal(f.Get("sig"), Sign(p.secret, ref, status, amount, cur)) {
		return nil, pay.ErrInvalidSignature
	}
	if ref != a.Ref {
		return nil, errors.New("demo: the callback is about another attempt")
	}
	inst, _ := strconv.Atoi(f.Get("installment"))
	switch status {
	case "approved":
		return &pay.Result{Status: pay.StatusSuccess, TxnID: "demo-" + ref, PaidAmount: amount, Currency: cur, Installment: inst}, nil
	case "declined":
		return &pay.Result{Status: pay.StatusFailure, Code: pay.CodeInsufficientFunds, ProviderCode: "51", ProviderMessage: "Insufficient funds (demo)"}, nil
	case "cancelled":
		return &pay.Result{Status: pay.StatusFailure, Code: pay.CodeCancelledByUser, ProviderMessage: "Cancelled on the demo page"}, nil
	}
	return nil, errors.New("demo: unknown status")
}

func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	bank.mu.Lock()
	defer bank.mu.Unlock()
	if r, ok := bank.approved[a.Ref]; ok {
		return &r, nil
	}
	return &pay.Result{Status: pay.StatusPending}, nil
}

func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	bank.mu.Lock()
	defer bank.mu.Unlock()
	if bank.refunds == nil {
		bank.refunds = map[string][]int64{}
	}
	bank.refunds[r.Attempt.Ref] = append(bank.refunds[r.Attempt.Ref], r.Amount)
	return &pay.Result{Status: pay.StatusSuccess, TxnID: "demo-rf-" + r.RefundRef, PaidAmount: r.Amount}, nil
}

// Refunds is what was refunded on an attempt (tests).
func Refunds(ref string) []int64 {
	bank.mu.Lock()
	defer bank.mu.Unlock()
	return append([]int64(nil), bank.refunds[ref]...)
}

// The hosted page --------------------------------------------------------------------------------------

var pageTpl = template.Must(template.New("page").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><meta name="robots" content="noindex">
<title>Demo bank</title><style>
body{font-family:system-ui,sans-serif;color:#1d2327;margin:0;padding:24px 16px;background:#f6f7f7}
main{max-width:440px;margin:0 auto;background:#fff;border:1px solid #dcdcde;border-radius:8px;padding:24px}
h1{font-size:1.2em;margin:0 0 .3em}.warn{color:#8a2424;font-size:.85em;margin:0 0 1em}
dl{display:grid;grid-template-columns:auto 1fr;gap:.4em 1em;margin:0 0 1.5em}dt{color:#646970}dd{margin:0;font-weight:600}
form{margin:0 0 .6em}button{width:100%;padding:.8em;font-size:1em;border-radius:6px;border:1px solid #8c8f94;background:#fff;cursor:pointer}
.ok{background:#00a32a;border-color:#00a32a;color:#fff}
</style></head><body><main>
<h1>Demo bank</h1><p class="warn">Development only. Nothing is charged and no card is asked for.</p>
<dl><dt>Amount</dt><dd>{{.Amount}} {{.Currency}}</dd><dt>Reference</dt><dd>{{.Ref}}</dd>
<dt>Installments</dt><dd>{{.Installment}}</dd>{{if .Desc}}<dt>For</dt><dd>{{.Desc}}</dd>{{end}}</dl>
{{range .Buttons}}<form method="post" action="/decide"><input type="hidden" name="t" value="{{$.Token}}">
<input type="hidden" name="status" value="{{.Status}}"><button type="submit" class="{{.Class}}">{{.Label}}</button></form>{{end}}
</main></body></html>`))

// backTpl sends the browser back to the store with the signed answer.
var backTpl = template.Must(template.New("back").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Demo bank</title></head>
<body><form id="f" method="post" action="{{.Action}}"{{if .Top}} target="_top"{{end}}>{{range .Fields}}<input type="hidden" name="{{index . 0}}" value="{{index . 1}}">{{end}}
<p>Returning to the store…</p><button type="submit">Continue</button></form>
<script>document.getElementById("f").submit();</script></body></html>`))

type button struct{ Status, Label, Class string }

func lookup(tok string) *session {
	bank.mu.Lock()
	defer bank.mu.Unlock()
	return bank.sessions[tok]
}

func servePage(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ss := lookup(r.Form.Get("t"))
	if ss == nil {
		http.Error(w, "unknown or expired payment", http.StatusNotFound)
		return
	}
	inst := "Single payment"
	if ss.installment > 1 {
		inst = strconv.Itoa(ss.installment)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = pageTpl.Execute(w, map[string]any{
		"Amount": pay.FormatMinor(ss.amount, ss.currency), "Currency": ss.currency, "Ref": ss.ref, "Installment": inst, "Desc": ss.desc,
		"Token":   r.Form.Get("t"),
		"Buttons": []button{{"approved", "Approve payment", "ok"}, {"declined", "Decline (insufficient funds)", ""}, {"cancelled", "Cancel", ""}},
	})
}

func serveDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	tok, status := r.PostForm.Get("t"), r.PostForm.Get("status")
	ss := lookup(tok)
	if ss == nil || (status != "approved" && status != "declined" && status != "cancelled") {
		http.Error(w, "unknown or expired payment", http.StatusNotFound)
		return
	}
	action := ss.failURL
	if status == "approved" {
		action = ss.returnURL
		bank.mu.Lock()
		bank.approved[ss.ref] = pay.Result{Status: pay.StatusSuccess, TxnID: "demo-" + ss.ref, PaidAmount: ss.amount, Currency: ss.currency,
			Installment: ss.installment}
		bank.mu.Unlock()
	}
	if u, err := url.Parse(action); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		http.Error(w, "bad return URL", http.StatusBadRequest)
		return
	}
	fields := DecisionFields(ss.secret, ss.ref, status, ss.amount, ss.currency, ss.installment)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = backTpl.Execute(w, map[string]any{"Action": action, "Top": ss.embed, "Fields": fields})
}

// DecisionFields are the fields the demo page posts back to the store.
func DecisionFields(secret, ref, status string, amount int64, currency string, installment int) [][2]string {
	return [][2]string{{"ref", ref}, {"status", status}, {"amount", strconv.FormatInt(amount, 10)}, {"currency", currency},
		{"installment", strconv.Itoa(max(installment, 1))}, {"sig", Sign(secret, ref, status, amount, currency)}}
}
