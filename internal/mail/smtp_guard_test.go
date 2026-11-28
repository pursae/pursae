package mail

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"wpgo/internal/mail/smtptest"
)

// The admin "Other SMTP" mailer passes a dial guard (Control); a refused address never connects,
// and failures surface as fixed categories without the remote banner.
func TestSMTPDialGuardAndCategories(t *testing.T) {
	refuse := errors.New("refused by guard")
	c := &SMTP{Host: "127.0.0.1", Port: "1", Control: func(string, string, syscall.RawConn) error { return refuse }}
	_, err := c.Send(context.Background(), "a@example.test", []string{"b@example.test"}, []byte("x"))
	if !errors.Is(err, refuse) || ErrorCategory(err) != "SMTP error: could not connect to the SMTP host." {
		t.Fatalf("err = %v (%q)", err, ErrorCategory(err))
	}

	// A non-SMTP service: its banner must not appear in the category.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			_, _ = conn.Write([]byte("+OK redis-secret-banner\r\n"))
			conn.Close()
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	_, err = (&SMTP{Host: h, Port: p}).Send(context.Background(), "a@example.test", []string{"b@example.test"}, []byte("x"))
	if cat := ErrorCategory(err); err == nil || strings.Contains(cat, "redis") || cat != "SMTP error: could not connect to the SMTP host." {
		t.Fatalf("err = %v, category %q", err, cat)
	}

	srv := &smtptest.Server{User: "u", Pass: "p", RejectTo: "no@example.test"}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	_, err = (&SMTP{Host: srv.Host(), Port: srv.Port(), Auth: true, User: "u", Pass: "wrong"}).Send(context.Background(), "a@example.test", []string{"b@example.test"}, []byte("x"))
	if ErrorCategory(err) != "SMTP error: authentication failed." {
		t.Fatalf("auth: %v → %q", err, ErrorCategory(err))
	}
	_, err = (&SMTP{Host: srv.Host(), Port: srv.Port(), Auth: true, User: "u", Pass: "p"}).Send(context.Background(), "a@example.test", []string{"no@example.test"}, []byte("x"))
	if ErrorCategory(err) != "SMTP error: the server rejected the message." {
		t.Fatalf("rcpt: %v → %q", err, ErrorCategory(err))
	}
}
