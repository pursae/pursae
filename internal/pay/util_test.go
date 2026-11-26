package pay

import "testing"

func TestFormatMinor(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		cur  string
		want string
	}{{34990, "TRY", "349.90"}, {5, "TRY", "0.05"}, {100, "USD", "1.00"}, {1500, "JPY", "1500"}, {-250, "EUR", "-2.50"}, {0, "TRY", "0.00"}} {
		if got := FormatMinor(tc.n, tc.cur); got != tc.want {
			t.Errorf("FormatMinor(%d, %s) = %q, want %q", tc.n, tc.cur, got, tc.want)
		}
	}
}

func TestParseMinor(t *testing.T) {
	for _, tc := range []struct {
		s    string
		cur  string
		want int64
	}{{"349.90", "TRY", 34990}, {"349,90", "TRY", 34990}, {"349.9", "TRY", 34990}, {"349", "TRY", 34900}, {" 0.05 ", "TRY", 5}, {"1500", "JPY", 1500}} {
		got, err := ParseMinor(tc.s, tc.cur)
		if err != nil || got != tc.want {
			t.Errorf("ParseMinor(%q, %s) = %d, %v; want %d", tc.s, tc.cur, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "-1.00", "1.234", "1e3", "1,234.50", "abc", ".50", "1.5.0", "+1", "12 3", "1500.5x"} {
		if _, err := ParseMinor(bad, "TRY"); err == nil {
			t.Errorf("ParseMinor(%q) accepted", bad)
		}
	}
	if _, err := ParseMinor("15.5", "JPY"); err == nil {
		t.Error("ParseMinor accepted a fraction for a zero-decimal currency")
	}
}

func TestEqual(t *testing.T) {
	if !Equal("abc", "abc") || Equal("abc", "abd") || Equal("", "") || Equal("a", "") {
		t.Error("Equal")
	}
}

func TestHMACSHA256Base64(t *testing.T) {
	// RFC 4231 test case 2.
	if got := HMACSHA256Base64("Jefe", "what do ya want for nothing?"); got != "W9zBRr9gdU5qBCQmCJV1x1oAPwidJzmDnexYuWTsOEM=" {
		t.Errorf("HMACSHA256Base64 = %q", got)
	}
}

func TestCurrencyCodes(t *testing.T) {
	if NumericCurrency("try") != "949" || CurrencyFromNumeric("949") != "TRY" || CurrencyFromNumeric("0949") != "TRY" || NumericCurrency("XXX") != "" {
		t.Error("currency codes")
	}
}
