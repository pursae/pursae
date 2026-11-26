package server

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFirewallRules(t *testing.T) {
	bad := []string{"/.env", "/.git/config", "/wp-config.php.bak", "/x?id=1+UNION+ALL+SELECT+1,2", "/x?q=%3Cscript%3Ealert(1)%3C/script%3E",
		"/x?q=%253Cscript%253E", "/x?f=../../../etc/passwd", "/x?a=${jndi:ldap://e/a}", "/x?s=pg_sleep(5)"}
	for _, u := range bad {
		r := httptest.NewRequest(http.MethodGet, "http://x"+u, nil)
		if fwMatch(r) == "" {
			t.Errorf("%s: not blocked", u)
		}
	}
	good := []string{"/", "/hello-world/", "/?s=union+station+select+menu", "/wp-json/wp/v2/posts?search=script", "/?p=12&preview=true", "/environment/"}
	for _, u := range good {
		r := httptest.NewRequest(http.MethodGet, "http://x"+u, nil)
		if rule := fwMatch(r); rule != "" {
			t.Errorf("%s: blocked (%s)", u, rule)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	r.Header.Set("User-Agent", "() { :; }; /bin/bash -c id")
	if fwMatch(r) == "" {
		t.Error("shellshock user agent not blocked")
	}
}

func TestFirewallLimiter(t *testing.T) {
	l := &fwLimiter{}
	now := time.Unix(6000, 0)
	for i := 1; i <= 5; i++ {
		if n := l.hit("1.2.3.4", now); n != i {
			t.Fatalf("hit %d = %d", i, n)
		}
	}
	if n := l.hit("5.6.7.8", now); n != 1 {
		t.Fatalf("other ip = %d", n)
	}
	if n := l.hit("1.2.3.4", now.Add(time.Minute)); n != 1 {
		t.Fatalf("new minute = %d", n)
	}
	if !l.firstLog("a") || l.firstLog("a") {
		t.Fatal("firstLog must be true once per minute")
	}
	for _, p := range []string{"/wp-includes/js/x.js", "/a/b.png", "/wp-admin/css/x.css"} {
		if !fwStaticRequest(httptest.NewRequest(http.MethodGet, p, nil)) {
			t.Errorf("%s should be static", p)
		}
	}
	if fwStaticRequest(httptest.NewRequest(http.MethodGet, "/wp-json/wp/v2/posts", nil)) || fwStaticRequest(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Error("pages must count")
	}
}

func TestIntegritySuspiciousNames(t *testing.T) {
	for _, n := range []string{"shell.php", "x.PHTML", "a.php.jpg", ".htaccess", ".user.ini", "run.sh"} {
		if bad, _ := asSuspiciousName(n); !bad {
			t.Errorf("%s not flagged", n)
		}
	}
	for _, n := range []string{"photo.jpg", "style.css", "theme.json", "index.html", "doc.pdf"} {
		if bad, _ := asSuspiciousName(n); bad {
			t.Errorf("%s flagged", n)
		}
	}
}

func TestHIBPKAnonymity(t *testing.T) {
	sum := sha1.Sum([]byte("password123"))
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprintf(w, "0000000000000000000000000000000000A:0\r\n%s:4242\r\n", h[5:])
	}))
	defer srv.Close()
	old := asHIBPEndpoint
	asHIBPEndpoint = srv.URL + "/range/"
	defer func() { asHIBPEndpoint = old }()
	if n := asPasswordBreached(context.Background(), "password123"); n != 4242 {
		t.Fatalf("count = %d", n)
	}
	if gotPath != "/range/"+h[:5] {
		t.Fatalf("sent %q, want only the 5-char prefix", gotPath)
	}
	if n := asPasswordBreached(context.Background(), "a-unique-Passw0rd-xyz"); n != 0 {
		t.Fatalf("unknown password count = %d", n)
	}
	asHIBPEndpoint = "http://127.0.0.1:1/range/"
	if n := asPasswordBreached(context.Background(), "password123"); n != 0 {
		t.Fatal("unreachable service must not block")
	}
}

func TestACFGoMenuRuleMatch(t *testing.T) {
	cases := []struct {
		v, id string
		locs  []string
		want  bool
	}{{"all", "3", nil, true}, {"3", "3", nil, true}, {"4", "3", nil, false},
		{"location/primary", "3", []string{"primary"}, true}, {"location/footer", "3", []string{"primary"}, false}}
	for _, c := range cases {
		if got := acfgoMenuRuleMatch(c.v, c.id, c.locs); got != c.want {
			t.Errorf("%v: %v", c, got)
		}
	}
}
