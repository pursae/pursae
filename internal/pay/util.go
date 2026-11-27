package pay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// MaxBody is the most a provider response or a callback body may be.
const MaxBody = 1 << 20

// Decimals is the number of minor-unit digits of a currency (2 for all currencies the providers here
// charge; the zero-decimal ones are listed).
func Decimals(currency string) int {
	switch strings.ToUpper(currency) {
	case "JPY", "KRW", "HUF", "TWD", "CLP", "ISK", "VND":
		return 0
	}
	return 2
}

// FormatMinor writes an amount in minor units as a decimal number with a dot: 34990 → "349.90".
func FormatMinor(amount int64, currency string) string {
	d := Decimals(currency)
	if d == 0 {
		return strconv.FormatInt(amount, 10)
	}
	neg := amount < 0
	if neg {
		amount = -amount
	}
	s := fmt.Sprintf("%d.%02d", amount/100, amount%100)
	if neg {
		s = "-" + s
	}
	return s
}

// ParseMinor reads a decimal amount ("349.90", "349,90", "349.9", "349") into minor units. It rejects
// anything that is not a plain non-negative number, so a provider's malformed amount is never read as
// a smaller or larger one.
func ParseMinor(s, currency string) (int64, error) {
	s = strings.TrimSpace(s)
	if strings.Count(s, ",") == 1 && !strings.Contains(s, ".") {
		s = strings.Replace(s, ",", ".", 1)
	}
	if s == "" {
		return 0, errors.New("pay: empty amount")
	}
	whole, frac, _ := strings.Cut(s, ".")
	d := Decimals(currency)
	if whole == "" || len(frac) > d || len(whole) > 15 {
		return 0, fmt.Errorf("pay: bad amount %q", s)
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("pay: bad amount %q", s)
		}
	}
	frac += strings.Repeat("0", d-len(frac))
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pay: bad amount %q", s)
	}
	return n, nil
}

// NumericCurrency is the ISO 4217 numeric code banks use ("TRY" → "949"), "" when not listed.
func NumericCurrency(currency string) string {
	switch strings.ToUpper(currency) {
	case "TRY":
		return "949"
	case "USD":
		return "840"
	case "EUR":
		return "978"
	case "GBP":
		return "826"
	case "JPY":
		return "392"
	case "RUB":
		return "643"
	case "CHF":
		return "756"
	}
	return ""
}

// CurrencyFromNumeric is the reverse of NumericCurrency, "" when not listed.
func CurrencyFromNumeric(code string) string {
	switch strings.TrimLeft(strings.TrimSpace(code), "0") {
	case "949":
		return "TRY"
	case "840":
		return "USD"
	case "978":
		return "EUR"
	case "826":
		return "GBP"
	case "392":
		return "JPY"
	case "643":
		return "RUB"
	case "756":
		return "CHF"
	}
	return ""
}

// HMACSHA256 is the raw HMAC-SHA256 of msg.
func HMACSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// HMACSHA256Base64 is the standard Base64 of the HMAC-SHA256 of msg.
func HMACSHA256Base64(key, msg string) string {
	return base64.StdEncoding.EncodeToString(HMACSHA256([]byte(key), []byte(msg)))
}

// Equal compares two signatures in constant time. An empty value never matches.
func Equal(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Do sends a request and returns the status and the body (at most MaxBody). A failure after the request
// may have reached the provider (timeout, connection reset while reading) is ErrUnknownOutcome wrapped
// with the cause; a failure before anything was sent (DNS, connection refused) is returned as is.
func Do(ctx context.Context, c *http.Client, req *http.Request) (int, []byte, error) {
	if c == nil {
		return 0, nil, errors.New("pay: no HTTP client")
	}
	// The host of the request is the fixed endpoint of a provider (Config.Origin), never a setting or a
	// callback value.
	res, err := c.Do(req.WithContext(ctx)) //nolint:gosec // G704: see above
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return 0, nil, err
		}
		var dns *net.DNSError
		if errors.As(err, &dns) {
			return 0, nil, err
		}
		return 0, nil, fmt.Errorf("%w: %w", ErrUnknownOutcome, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxBody+1))
	if err != nil {
		return res.StatusCode, nil, fmt.Errorf("%w: %w", ErrUnknownOutcome, err)
	}
	if len(body) > MaxBody {
		return res.StatusCode, nil, errors.New("pay: provider response too large")
	}
	return res.StatusCode, body, nil
}

// PostForm posts application/x-www-form-urlencoded fields.
func PostForm(ctx context.Context, c *http.Client, endpoint string, form url.Values, header http.Header) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return Do(ctx, c, req)
}

// PostBody posts a body with a content type (JSON, XML).
func PostBody(ctx context.Context, c *http.Client, endpoint, contentType string, body []byte, header http.Header) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", contentType)
	return Do(ctx, c, req)
}

// Trunc cuts s to at most n runes, for the length limits of provider fields.
func Trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Value reads a setting, trimmed.
func (c Config) Value(id string) string { return strings.TrimSpace(c.Values[id]) }

// Bool reads a checkbox setting.
func (c Config) Bool(id string) bool { return c.Value(id) == "yes" }

// Require returns an error naming the first empty setting among ids.
func (c Config) Require(ids ...string) error {
	for _, id := range ids {
		if c.Value(id) == "" {
			return fmt.Errorf("pay: setting %q is required", id)
		}
	}
	return nil
}

// Origin returns BaseURL when a test set it, otherwise the live or test origin by mode.
func (c Config) Origin(live, test string) string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	if c.Test {
		return test
	}
	return live
}
