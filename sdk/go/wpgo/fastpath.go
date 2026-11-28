package wpgo

import (
	"bytes"
	"unicode/utf8"
)

// Lean paths for the host calls plugins make most often: get_option (GetOption, Setting*,
// OptionBool) and translate (T, X).
//
// Inside a Wasm guest, encoding/json is most of the cost of a host call: the call itself is a few
// microseconds, but the reflection-based encoder and decoder run slowly as Go Wasm code, and the
// host runtime's termination check (which stops a plugin that overruns its time budget) adds an exit
// to the host on every loop and function entry. One GetOption used to cost an encode plus three
// decodes. Here the request is written by hand and the response is matched against the exact
// bytes the host writes for the common case (a successful call with a plain string value); any
// other response — an error, a non-string value, a string with escapes — goes through
// encoding/json as before, so the results are the same either way.

// appendJSONString appends s as a JSON string. Invalid UTF-8 is written as is: the host's
// encoding/json decoder turns it into U+FFFD, as json.Marshal would have before sending.
func appendJSONString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		b = append(b, s[start:i]...)
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// fastString matches raw against prefix + a JSON string without escapes + suffix and returns the
// string. ok is false for anything else (the caller then decodes raw with encoding/json).
func fastString(raw []byte, prefix, suffix string) (string, bool) {
	if !bytes.HasPrefix(raw, []byte(prefix)) || !bytes.HasSuffix(raw, []byte(suffix)) || len(raw) < len(prefix)+len(suffix)+2 {
		return "", false
	}
	body := raw[len(prefix) : len(raw)-len(suffix)]
	if body[0] != '"' || body[len(body)-1] != '"' {
		return "", false
	}
	s := body[1 : len(body)-1]
	for _, c := range s {
		if c == '"' || c == '\\' || c < 0x20 {
			return "", false
		}
	}
	if !utf8.Valid(s) {
		return "", false
	}
	return string(s), true
}

// Exact responses of the host (internal/plugins: hostGetOption and hostTranslate return maps,
// which encoding/json writes with sorted keys).
const (
	optionFoundPrefix = `{"ok":true,"result":{"found":true,"value":`
	optionMissing     = `{"ok":true,"result":{"found":false,"value":null}}`
	translatePrefix   = `{"ok":true,"result":{"text":`
	resultSuffix      = `}}`
)

// getOptionRequest is the get_option request for name.
func getOptionRequest(name string) []byte {
	return append(appendJSONString(append(make([]byte, 0, len(name)+12), `{"name":`...), name), '}')
}

// translateRequest is the translate request for text and context.
func translateRequest(text, context string) []byte {
	b := make([]byte, 0, len(text)+len(context)+24)
	b = appendJSONString(append(b, `{"text":`...), text)
	b = appendJSONString(append(b, `,"context":`...), context)
	return append(b, '}')
}
