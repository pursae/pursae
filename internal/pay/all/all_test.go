package all

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"wpgo/internal/pay"
)

// Every provider's Spec must be complete enough for the bridge and Settings › Payments, and must not
// declare anything that looks like a card field.
func TestSpecs(t *testing.T) {
	id := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	card := regexp.MustCompile(`(?i)card.?(number|no)|pan$|cvv|cvc|expir`)
	seen := map[string]bool{}
	for _, sp := range Specs() {
		if !id.MatchString(sp.ID) || seen[sp.ID] {
			t.Errorf("%q: bad or duplicate id", sp.ID)
		}
		seen[sp.ID] = true
		if sp.Title == "" || sp.Description == "" || sp.New == nil {
			t.Errorf("%s: title, description and New are required", sp.ID)
		}
		if sp.Region != pay.RegionTR && sp.Region != pay.RegionGlobal {
			t.Errorf("%s: region %q", sp.ID, sp.Region)
		}
		if sp.Kind != pay.KindPSP && sp.Kind != pay.KindBank && sp.Kind != pay.KindWallet {
			t.Errorf("%s: kind %q", sp.ID, sp.Kind)
		}
		fields := map[string]bool{}
		for _, f := range sp.Fields {
			if !id.MatchString(f.ID) || fields[f.ID] || f.Title == "" {
				t.Errorf("%s: field %q: bad id, duplicate or no title", sp.ID, f.ID)
			}
			fields[f.ID] = true
			if card.MatchString(f.ID) {
				t.Errorf("%s: field %q looks like card data", sp.ID, f.ID)
			}
			if f.Secret && f.Type != "password" {
				t.Errorf("%s: secret field %q must be a password field", sp.ID, f.ID)
			}
			switch f.ID {
			case "enabled", "title", "description", "testmode", "installments", "max_installment":
				t.Errorf("%s: field %q is one the bridge adds itself", sp.ID, f.ID)
			}
		}
		for _, h := range append(append(append([]string{}, sp.Hosts.Frame...), sp.Hosts.Script...), sp.Hosts.Form...) {
			if sp.ID == "demo" && h == "http://127.0.0.1" {
				continue // the development-only demo bank (build tag paydemo) is served on loopback
			}
			if !strings.HasPrefix(h, "https://") || strings.Count(h, "/") != 2 {
				t.Errorf("%s: host %q must be an https origin", sp.ID, h)
			}
		}
		// Without settings a provider must refuse to be built, and never panic.
		if p, err := sp.New(pay.Config{Values: map[string]string{}, HTTP: http.DefaultClient}); err == nil || p != nil {
			t.Errorf("%s: New accepted empty settings", sp.ID)
		}
	}
	if len(seen) < 10 {
		t.Errorf("only %d providers registered", len(seen))
	}
}
