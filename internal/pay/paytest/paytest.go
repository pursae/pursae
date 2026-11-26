// Package paytest is the shared body of the provider sandbox tests (sandbox_test.go in each provider
// package, build tag "sandbox"). It runs a provider against its real test endpoint with the merchant's
// test credentials, which come only from environment variables; see docs/developer/payments-sandbox.md.
//
// It holds no credentials and no card data. Normal builds never run it: only the sandbox test files
// import it.
package paytest

import (
	"context"
	"crypto/rand"
	"errors"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"wpgo/internal/pay"
)

// Env is the name of a sandbox variable: PAY_SANDBOX_<SPEC ID>_<NAME>, upper-cased ("paytr" +
// "merchant_id" is PAY_SANDBOX_PAYTR_MERCHANT_ID, "est_isbank" + "client_id" PAY_SANDBOX_EST_ISBANK_CLIENT_ID).
func Env(spec pay.Spec, name string) string {
	return "PAY_SANDBOX_" + strings.ToUpper(spec.ID) + "_" + strings.ToUpper(name)
}

func currencyOf(spec pay.Spec) string {
	if spec.ID == "paypal" {
		return "USD"
	}
	return "TRY"
}

// provider builds the spec's provider in test mode from the environment, or skips the test naming the
// required variables that are missing.
func provider(t *testing.T, spec pay.Spec) (pay.Provider, map[string]string) {
	t.Helper()
	values := map[string]string{}
	var missing []string
	for _, f := range spec.Fields {
		v := strings.TrimSpace(os.Getenv(Env(spec, f.ID)))
		if v != "" {
			values[f.ID] = v
		} else if f.Required {
			missing = append(missing, Env(spec, f.ID))
		}
	}
	if len(missing) > 0 {
		t.Skip("sandbox credentials not set: " + strings.Join(missing, ", "))
	}
	p, err := spec.New(pay.Config{Values: values, Test: true, HTTP: &http.Client{Timeout: 30 * time.Second}})
	if err != nil {
		t.Fatalf("%s: building the provider: %v", spec.ID, err)
	}
	return p, values
}

// ref is a fresh random attempt reference: 20 characters of [A-Za-z0-9].
func ref(prefix string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 20-len(prefix))
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return prefix + string(b)
}

func returnBase() string {
	if v := strings.TrimRight(os.Getenv("PAY_SANDBOX_RETURN_BASE"), "/"); v != "" {
		return v
	}
	return "https://example.test"
}

func clientIP() string {
	if v := strings.TrimSpace(os.Getenv("PAY_SANDBOX_CLIENT_IP")); v != "" {
		return v
	}
	return "203.0.113.10" // documentation range; set PAY_SANDBOX_CLIENT_IP where the provider wants a real address
}

func startRequest(spec pay.Spec, r string) pay.StartRequest {
	cur := currencyOf(spec)
	base := returnBase() + "/pay/" + r
	addr := pay.Address{Name: "Test Buyer", Line1: "1 Test Street", City: "Istanbul", State: "Istanbul", Postcode: "34000", Country: "TR"}
	return pay.StartRequest{
		Ref: r, OrderNumber: "T" + r[:8], Amount: 1000, Currency: cur, Installment: 0, MaxInstallment: 1,
		Description: "Sandbox test order " + r[:8],
		Customer:    pay.Customer{ID: "", Email: "buyer@example.test", FirstName: "Test", LastName: "Buyer", Phone: "+905550000000", IP: clientIP()},
		Billing:     addr, Shipping: addr,
		Items:     []pay.Item{{ID: "sku-1", Name: "Sandbox item", Category: "Test", Quantity: 1, UnitPrice: 1000, Virtual: true}},
		ReturnURL: base + "/return", FailURL: base + "/fail", NotifyURL: base + "/notify",
		Locale: "en", ExpiresIn: 30 * time.Minute,
	}
}

// onHost reports whether u is an https URL on one of the origins (any https URL when there are none).
func onHost(raw string, origins []string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	if len(origins) == 0 {
		return true
	}
	for _, o := range origins {
		if ou, err := url.Parse(o); err == nil && ou.Scheme == u.Scheme && ou.Host == u.Host {
			return true
		}
	}
	return false
}

var secretName = regexp.MustCompile(`(?i)pass|secret|key|hash|sign|token|salt|pwd`)

// Start runs TestSandboxStart for each spec: Start, then checks the shape of the result and logs where a
// person can pay with the provider's test card.
func Start(t *testing.T, specs ...pay.Spec) {
	for _, spec := range specs {
		t.Run(spec.ID, func(t *testing.T) {
			p, values := provider(t, spec)
			req := startRequest(spec, ref("S"))
			res, err := p.Start(context.Background(), req)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			n := 0
			var all []string
			if res.Redirect != "" {
				n++
				all = append(all, res.Redirect)
				if !onHost(res.Redirect, nil) {
					t.Errorf("Redirect is not an https URL: %s", pay.Trunc(res.Redirect, 80))
				}
			}
			if res.Form != nil {
				n++
				all = append(all, res.Form.Action)
				for _, f := range res.Form.Fields {
					all = append(all, f[1])
				}
				if !onHost(res.Form.Action, spec.Hosts.Form) {
					t.Errorf("Form action %s is not an https URL on %v", res.Form.Action, spec.Hosts.Form)
				}
			}
			if res.Embed != nil {
				n++
				all = append(all, res.Embed.FrameURL)
				if !onHost(res.Embed.FrameURL, spec.Hosts.Frame) {
					t.Errorf("Embed frame %s is not an https URL on %v", res.Embed.FrameURL, spec.Hosts.Frame)
				}
				for _, s := range res.Embed.Scripts {
					if !onHost(s, spec.Hosts.Script) {
						t.Errorf("Embed script %s is not an https URL on %v", s, spec.Hosts.Script)
					}
				}
			}
			if n != 1 {
				t.Fatalf("want exactly one of Redirect / Form / Embed, got %d", n)
			}
			// What goes to the customer's browser must never carry a credential.
			for _, f := range spec.Fields {
				if v := values[f.ID]; f.Secret && len(v) >= 4 {
					for _, s := range all {
						if strings.Contains(s, v) {
							t.Errorf("a secret credential (%s) appears in what is sent to the browser", f.ID)
						}
					}
				}
			}
			t.Logf("ref %s, provider ref %q: open the page below and pay with the provider's TEST card, then capture the callback (docs/developer/payments-sandbox.md)", req.Ref, res.ProviderRef)
			switch {
			case res.Redirect != "":
				t.Logf("open: %s", res.Redirect)
			case res.Embed != nil:
				t.Logf("open the frame: %s", res.Embed.FrameURL)
			default:
				t.Logf("POST form to %s with fields:", res.Form.Action)
				for _, f := range res.Form.Fields {
					v := f[1]
					if secretName.MatchString(f[0]) || len(v) > 80 {
						v = "(" + strconv.Itoa(len(v)) + " chars, hidden)"
					}
					t.Logf("  %s = %s", f[0], v)
				}
				if dir := os.Getenv("PAY_SANDBOX_OUT_DIR"); dir != "" {
					path := filepath.Join(dir, spec.ID+"-start.html")
					if err := os.WriteFile(path, []byte(formPage(res.Form)), 0o600); err != nil {
						t.Errorf("writing %s: %v", path, err)
					} else {
						t.Logf("a self-submitting page with the full form was written to %s (open it in a browser)", path)
					}
				} else {
					t.Log("set PAY_SANDBOX_OUT_DIR to a folder to get a self-submitting HTML page of this form")
				}
			}
		})
	}
}

// formPage is an HTML page that posts the form to the provider when opened.
func formPage(f *pay.Form) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8"><title>Sandbox payment</title><form method="post" action="` + html.EscapeString(f.Action) + `">`)
	for _, kv := range f.Fields {
		b.WriteString(`<input type="hidden" name="` + html.EscapeString(kv[0]) + `" value="` + html.EscapeString(kv[1]) + `">`)
	}
	b.WriteString(`<noscript><button>Continue to the provider</button></noscript></form><script>document.forms[0].submit()</script>`)
	return b.String()
}

// QueryUnknown runs TestSandboxQueryUnknown: a reference that was never paid must not read as paid.
func QueryUnknown(t *testing.T, specs ...pay.Spec) {
	for _, spec := range specs {
		t.Run(spec.ID, func(t *testing.T) {
			p, _ := provider(t, spec)
			a := pay.Attempt{Ref: ref("Q"), Amount: 1000, Currency: currencyOf(spec), Test: true, CreatedAt: time.Now()}
			res, err := p.Query(context.Background(), a)
			var pe *pay.Error
			switch {
			case errors.Is(err, pay.ErrNotSupported):
				t.Log("Query is not supported by this provider")
			case errors.As(err, &pe):
				if pe.Code == pay.CodeConfiguration || pe.Code == pay.CodeProviderDown {
					t.Fatalf("Query: %v (check the credentials and that the terminal is enabled for test mode)", err)
				}
				t.Logf("the provider rejected the unknown reference: %v", err)
			case err != nil:
				t.Fatalf("Query: %v", err)
			case res.Status == pay.StatusSuccess:
				t.Fatalf("a reference that was never paid reads as success: %+v", res)
			default:
				t.Logf("status %s, code %s, provider %s %s", res.Status, res.Code, res.ProviderCode, res.ProviderMessage)
			}
		})
	}
}

// callback reads the captured callback of the spec from PAY_SANDBOX_<ID>_CALLBACK: a file holding the raw
// body, JSON when it starts with { or [, otherwise a urlencoded form. _CALLBACK_KIND (return, fail,
// notify) says which URL it was posted to; the default is notify for JSON and return for a form.
func callback(t *testing.T, spec pay.Spec, body []byte) pay.CallbackInput {
	t.Helper()
	in := pay.CallbackInput{Kind: pay.CallbackReturn, Method: http.MethodPost, Header: http.Header{}, Body: body}
	if s := strings.TrimSpace(string(body)); strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		in.Kind = pay.CallbackNotify
		in.Header.Set("Content-Type", "application/json")
	} else {
		form, err := url.ParseQuery(strings.TrimSpace(string(body)))
		if err != nil {
			t.Fatalf("the captured callback is neither JSON nor a urlencoded form: %v", err)
		}
		in.Form = form
		in.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	switch k := pay.CallbackKind(strings.ToLower(os.Getenv(Env(spec, "CALLBACK_KIND")))); k {
	case pay.CallbackReturn, pay.CallbackFail, pay.CallbackNotify:
		in.Kind = k
	}
	return in
}

// flip changes one letter or digit in the middle of the body, so a signature over it cannot match.
func flip(body []byte) []byte {
	out := append([]byte(nil), body...)
	for i := len(out) / 2; i < len(out); i++ {
		c := out[i]
		switch {
		case c >= 'a' && c <= 'y', c >= 'A' && c <= 'Y', c >= '0' && c <= '8':
			out[i] = c + 1
			return out
		case c == 'z' || c == 'Z' || c == '9':
			out[i] = c - 1
			return out
		}
	}
	return out
}

// Complete runs TestSandboxComplete: a callback captured from a real test payment verifies and carries
// the amount, and the same callback with one character changed does not.
func Complete(t *testing.T, specs ...pay.Spec) {
	for _, spec := range specs {
		t.Run(spec.ID, func(t *testing.T) {
			file := os.Getenv(Env(spec, "CALLBACK"))
			if file == "" {
				t.Skip("no captured callback: set " + Env(spec, "CALLBACK") + " (file), " + Env(spec, "REF") + ", " + Env(spec, "AMOUNT") + " and " + Env(spec, "PROVIDER_REF"))
			}
			p, _ := provider(t, spec)
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("reading the captured callback: %v", err)
			}
			amount, err := strconv.ParseInt(os.Getenv(Env(spec, "AMOUNT")), 10, 64)
			if err != nil || amount <= 0 {
				t.Fatalf("%s must be the amount in minor units (1000 for 10.00): %v", Env(spec, "AMOUNT"), err)
			}
			a := pay.Attempt{Ref: os.Getenv(Env(spec, "REF")), ProviderRef: os.Getenv(Env(spec, "PROVIDER_REF")), Amount: amount,
				Currency: currencyOf(spec), Test: true, CreatedAt: time.Now()}
			if a.Ref == "" {
				t.Fatalf("%s is not set", Env(spec, "REF"))
			}
			in := callback(t, spec, body)
			if got := p.CallbackRef(in); got != a.Ref {
				t.Errorf("CallbackRef = %q, want %q", got, a.Ref)
			}
			res, err := p.Complete(context.Background(), in, a)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if res.Status != pay.StatusSuccess || res.PaidAmount < amount {
				t.Fatalf("Complete: status %s, paid %d (want success and at least %d); code %s, provider %s %s",
					res.Status, res.PaidAmount, amount, res.Code, res.ProviderCode, res.ProviderMessage)
			}
			t.Logf("success: paid %d %s, txn %s, installment %d", res.PaidAmount, res.Currency, res.TxnID, res.Installment)

			bad, err := p.Complete(context.Background(), callback(t, spec, flip(body)), a)
			if err == nil && bad.Status == pay.StatusSuccess {
				t.Error("a callback with one changed character was accepted as success")
			}
		})
	}
}

// Refund runs TestSandboxRefund: it refunds 1.00 of a test payment made earlier.
func Refund(t *testing.T, specs ...pay.Spec) {
	for _, spec := range specs {
		t.Run(spec.ID, func(t *testing.T) {
			r := os.Getenv(Env(spec, "REFUND_REF"))
			if r == "" {
				t.Skip("no payment to refund: set " + Env(spec, "REFUND_REF") + ", " + Env(spec, "REFUND_TXN") + " and " + Env(spec, "REFUND_AMOUNT"))
			}
			p, _ := provider(t, spec)
			amount, err := strconv.ParseInt(os.Getenv(Env(spec, "REFUND_AMOUNT")), 10, 64)
			if err != nil || amount < 100 {
				t.Fatalf("%s must be the paid amount in minor units (at least 100): %v", Env(spec, "REFUND_AMOUNT"), err)
			}
			a := pay.Attempt{Ref: r, ProviderRef: os.Getenv(Env(spec, "REFUND_PROVIDER_REF")), Amount: amount, Currency: currencyOf(spec), Test: true,
				CreatedAt: time.Now(), PaidAmount: amount, TxnID: os.Getenv(Env(spec, "REFUND_TXN"))}
			res, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: a, Amount: 100, Full: amount == 100, RefundRef: ref("R"), Reason: "sandbox test"})
			if err != nil {
				t.Fatalf("Refund: %v", err)
			}
			t.Logf("refund: status %s, txn %s, amount %d %s, code %s, provider %s %s", res.Status, res.TxnID, res.PaidAmount, res.Currency, res.Code, res.ProviderCode, res.ProviderMessage)
			if res.Status != pay.StatusSuccess && res.Status != pay.StatusPending {
				t.Errorf("refund status %s", res.Status)
			}
		})
	}
}
