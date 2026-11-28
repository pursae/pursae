//go:build !(wasip1 && wasm)

package wpgo

import (
	"encoding/json"
	"testing"
)

var fastpathStrings = []string{
	"", "Bench", "#ff0000", `a"b`, `back\slash`, "line\nbreak\r\t", "\x00\x1f\x7f", "<b>&amp;</b>",
	"ünïcødé ✓ 😀", "  ", "bad\xffutf8", "\xed\xa0\x80", "/path/?q=1&r=2",
}

// The hand-written requests decode on the host to the same values json.Marshal would have sent.
func TestFastpathRequests(t *testing.T) {
	for _, s := range fastpathStrings {
		var got struct{ Name, Text, Context string }
		if err := json.Unmarshal(getOptionRequest(s), &got); err != nil {
			t.Fatalf("%q: %v (%s)", s, err, getOptionRequest(s))
		}
		want, _ := json.Marshal(s)
		var w string
		_ = json.Unmarshal(want, &w)
		if got.Name != w {
			t.Errorf("get_option %q: host reads %q, want %q", s, got.Name, w)
		}
		got = struct{ Name, Text, Context string }{}
		if err := json.Unmarshal(translateRequest(s, s+"x"), &got); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got.Text != w || got.Context != w+"x" {
			t.Errorf("translate %q: host reads %q / %q", s, got.Text, got.Context)
		}
	}
}

// GetOption and T give the same results whether the response takes the fast path or the
// encoding/json one, for every value shape the host can send.
func TestFastpathResponses(t *testing.T) {
	values := []any{nil, true, 3.5, map[string]any{"a": 1}, []any{"x"}}
	for _, s := range fastpathStrings {
		values = append(values, s)
	}
	for _, v := range values {
		for _, found := range []bool{true, false} {
			env, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"found": found, "value": v}})
			restore := SetHostForTest(func(name string, _ []byte) ([]byte, error) {
				if name == "translate" {
					s, _ := v.(string)
					return json.Marshal(map[string]any{"ok": true, "result": map[string]string{"text": s}})
				}
				return env, nil
			})
			got, gotFound, err := GetOption("o")
			wantFound, want := found, ""
			if found {
				raw, _ := json.Marshal(v)
				if json.Unmarshal(raw, &want) != nil {
					want = string(raw)
				}
			}
			if err != nil || gotFound != wantFound || got != want {
				t.Errorf("GetOption with %s: got %q %v %v, want %q %v", env, got, gotFound, err, want, wantFound)
			}
			if s, ok := v.(string); ok && found {
				var w string
				raw, _ := json.Marshal(s)
				_ = json.Unmarshal(raw, &w)
				if w == "" {
					w = "in"
				}
				if got := T("in"); got != w {
					t.Errorf("T with %q: got %q, want %q", s, got, w)
				}
			}
			restore()
		}
	}
	restore := SetHostForTest(func(string, []byte) ([]byte, error) {
		return []byte(`{"ok":false,"code":"permission_denied","error":"no"}`), nil
	})
	defer restore()
	if _, _, err := GetOption("o"); !IsPermissionDenied(err) {
		t.Errorf("error response: %v", err)
	}
	if got := T("in"); got != "in" {
		t.Errorf("T on error: %q", got)
	}
}

func TestFastStringRejects(t *testing.T) {
	for _, raw := range []string{
		optionFoundPrefix + `"a\"b"}}`, optionFoundPrefix + `"a"b"}}`, optionFoundPrefix + `"a"}}x`,
		optionFoundPrefix + `1}}`, optionFoundPrefix + `"}}`, optionFoundPrefix + "\"\xff\"}}", optionFoundPrefix + "\"a\nb\"}}",
	} {
		if v, ok := fastString([]byte(raw), optionFoundPrefix, resultSuffix); ok {
			t.Errorf("%q accepted as %q", raw, v)
		}
	}
}
