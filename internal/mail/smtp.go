package mail

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"syscall"
	"time"
)

// Transport takes over a Mailer's delivery (see Mailer.Transport).
type Transport interface {
	Deliver(ctx context.Context, m *Mailer, msg Message) error
}

// SMTP is the "Other SMTP" mailer of WPGO Mail SMTP: an SMTP client with WP Mail SMTP's options
// (encryption none/SSL/TLS, Auto TLS, optional authentication) and a transcript for the Email
// Test screen. Credentials and message bodies never reach the transcript.
type SMTP struct {
	Host, Port string
	Encryption string // "none", "ssl" (implicit TLS) or "tls" (STARTTLS required)
	AutoTLS    bool   // with "none": upgrade with STARTTLS when the server offers it
	Auth       bool
	User, Pass string
	LocalName  string      // EHLO name, "localhost" when empty
	TLSConfig  *tls.Config // nil: verify the server name
	Debug      io.Writer   // SMTP transcript ("C: …" / "S: …"), optional
	Timeout    time.Duration
	// Control, when set, runs on every connection's resolved address (net.Dialer.Control): the
	// admin-configured SMTP mailer passes the private-address guard here so the host cannot be
	// used to probe the server's internal network, DNS rebinding included.
	Control func(network, address string, c syscall.RawConn) error
}

// SMTP failure stages, for SendError.
const (
	StageConnect  = "connect"  // TCP connect, implicit TLS, or the server greeting
	StageTLS      = "tls"      // STARTTLS
	StageAuth     = "auth"     // AUTH
	StageRejected = "rejected" // HELO, MAIL, RCPT, DATA
)

// SendError is a failed SMTP.Send with the stage it failed in, so callers can show a fixed
// category instead of raw dial errors and server banners (ErrorCategory).
type SendError struct {
	Stage string
	Err   error
}

func (e *SendError) Error() string { return e.Err.Error() }
func (e *SendError) Unwrap() error { return e.Err }

// ErrorCategory is a fixed, banner-free description of an SMTP.Send error: connection failed,
// timed out, TLS failed, authentication failed or rejected.
func ErrorCategory(err error) string {
	if err == nil {
		return ""
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "SMTP error: the connection timed out."
	}
	var cv *tls.CertificateVerificationError
	var rh tls.RecordHeaderError
	if errors.As(err, &cv) || errors.As(err, &rh) {
		return "SMTP error: the TLS negotiation failed."
	}
	var se *SendError
	if !errors.As(err, &se) {
		return "SMTP error: could not connect to the SMTP host."
	}
	switch se.Stage {
	case StageTLS:
		return "SMTP error: the TLS negotiation failed."
	case StageAuth:
		return "SMTP error: authentication failed."
	case StageRejected:
		return "SMTP error: the server rejected the message."
	}
	return "SMTP error: could not connect to the SMTP host."
}

type smtpSession struct {
	s    *SMTP
	conn net.Conn
	tp   *textproto.Conn
	ext  map[string]string
}

func (x *smtpSession) logf(format string, a ...any) {
	if x.s.Debug != nil {
		fmt.Fprintf(x.s.Debug, format+"\n", a...)
	}
}

// cmd sends one command and reads its reply; shown replaces the command in the transcript.
func (x *smtpSession) cmd(expect int, shown, format string, args ...any) (int, string, error) {
	line := fmt.Sprintf(format, args...)
	if shown == "" {
		shown = line
	}
	x.logf("C: %s", shown)
	id, err := x.tp.Cmd("%s", line)
	if err != nil {
		return 0, "", err
	}
	x.tp.StartResponse(id)
	defer x.tp.EndResponse(id)
	code, msg, err := x.tp.ReadResponse(expect)
	x.logResponse(code, msg)
	return code, msg, err
}

func (x *smtpSession) logResponse(code int, msg string) {
	for _, l := range strings.Split(msg, "\n") {
		x.logf("S: %d %s", code, l)
	}
}

func (x *smtpSession) hello() error {
	name := x.s.LocalName
	if name == "" {
		name = "localhost"
	}
	_, msg, err := x.cmd(250, "", "EHLO %s", name)
	if err != nil {
		if _, _, err = x.cmd(250, "", "HELO %s", name); err != nil {
			return err
		}
		x.ext = map[string]string{}
		return nil
	}
	x.ext = map[string]string{}
	for _, l := range strings.Split(msg, "\n")[1:] {
		k, v, _ := strings.Cut(l, " ")
		x.ext[strings.ToUpper(k)] = v
	}
	return nil
}

func (s *SMTP) tlsConfig() *tls.Config {
	if s.TLSConfig != nil {
		return s.TLSConfig
	}
	return &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
}

// Send delivers raw (an RFC 5322 message) from the envelope sender to the recipients and returns
// the server's final reply (often carrying the queue id).
func (s *SMTP) Send(ctx context.Context, from string, rcpt []string, raw []byte) (string, error) {
	stage := StageConnect
	msg, err := s.send(ctx, from, rcpt, raw, &stage)
	if err != nil && s.Host != "" {
		return "", &SendError{Stage: stage, Err: err}
	}
	return msg, err
}

func (s *SMTP) send(ctx context.Context, from string, rcpt []string, raw []byte, stage *string) (string, error) {
	if s.Host == "" {
		return "", errors.New("SMTP host is not set")
	}
	port := s.Port
	if port == "" {
		port = map[string]string{"ssl": "465", "tls": "587"}[s.Encryption]
		if port == "" {
			port = "25"
		}
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	addr := net.JoinHostPort(s.Host, port)
	x := &smtpSession{s: s}
	x.logf("Connecting to %s (encryption: %s)", addr, map[bool]string{true: s.Encryption, false: "none"}[s.Encryption != ""])
	d := &net.Dialer{Timeout: 15 * time.Second, Control: s.Control}
	var err error
	if s.Encryption == "ssl" {
		x.conn, err = (&tls.Dialer{NetDialer: d, Config: s.tlsConfig()}).DialContext(ctx, "tcp", addr)
	} else {
		x.conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		x.logf("Connection failed: %v", err)
		return "", fmt.Errorf("could not connect to the SMTP host %s: %w", addr, err)
	}
	defer x.conn.Close()
	_ = x.conn.SetDeadline(time.Now().Add(timeout))
	x.tp = textproto.NewConn(x.conn)
	code, msg, err := x.tp.ReadResponse(220)
	x.logResponse(code, msg)
	if err != nil {
		return "", err
	}
	*stage = StageRejected
	if err := x.hello(); err != nil {
		return "", err
	}
	_, canTLS := x.ext["STARTTLS"]
	*stage = StageTLS
	if s.Encryption == "tls" && !canTLS {
		return "", errors.New("the SMTP server does not support STARTTLS; choose another encryption")
	}
	if s.Encryption == "tls" || (s.Encryption != "ssl" && s.AutoTLS && canTLS) {
		if _, _, err := x.cmd(220, "", "STARTTLS"); err != nil {
			return "", err
		}
		tc := tls.Client(x.conn, s.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			x.logf("TLS handshake failed: %v", err)
			return "", fmt.Errorf("STARTTLS: %w", err)
		}
		x.logf("TLS negotiated (%s)", tls.VersionName(tc.ConnectionState().Version))
		x.conn = tc
		x.tp = textproto.NewConn(tc)
		if err := x.hello(); err != nil {
			return "", err
		}
	}
	if s.Auth && s.User != "" {
		*stage = StageAuth
		mechs := strings.ToUpper(x.ext["AUTH"])
		switch {
		case strings.Contains(mechs, "PLAIN") || mechs == "":
			resp := base64.StdEncoding.EncodeToString([]byte("\x00" + s.User + "\x00" + s.Pass))
			if _, _, err := x.cmd(235, "AUTH PLAIN [credentials hidden]", "AUTH PLAIN %s", resp); err != nil {
				return "", fmt.Errorf("SMTP authentication failed: %w", err)
			}
		case strings.Contains(mechs, "LOGIN"):
			if _, _, err := x.cmd(334, "", "AUTH LOGIN"); err != nil {
				return "", fmt.Errorf("SMTP authentication failed: %w", err)
			}
			if _, _, err := x.cmd(334, "[username hidden]", "%s", base64.StdEncoding.EncodeToString([]byte(s.User))); err != nil {
				return "", fmt.Errorf("SMTP authentication failed: %w", err)
			}
			if _, _, err := x.cmd(235, "[password hidden]", "%s", base64.StdEncoding.EncodeToString([]byte(s.Pass))); err != nil {
				return "", fmt.Errorf("SMTP authentication failed: %w", err)
			}
		default:
			return "", fmt.Errorf("the SMTP server offers no supported authentication mechanism (%s)", mechs)
		}
	}
	*stage = StageRejected
	if _, _, err := x.cmd(250, "", "MAIL FROM:<%s>", from); err != nil {
		return "", err
	}
	for _, r := range rcpt {
		if _, _, err := x.cmd(25, "", "RCPT TO:<%s>", r); err != nil {
			return "", err
		}
	}
	if _, _, err := x.cmd(354, "", "DATA"); err != nil {
		return "", err
	}
	w := x.tp.DotWriter()
	if _, err := w.Write(raw); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	x.logf("C: [message, %d bytes]", len(raw))
	code, msg, err = x.tp.ReadResponse(250)
	x.logResponse(code, msg)
	if err != nil {
		return "", err
	}
	_, _, _ = x.cmd(221, "", "QUIT")
	return msg, nil
}
