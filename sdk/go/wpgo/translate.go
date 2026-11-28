package wpgo

import "encoding/json"

// T returns text in the language of the current request (WordPress' __()). With Pursae Multi
// Language on, the string is listed on Languages › Translations under the plugin (domain = the
// plugin's slug) and translated there or by an installed language pack. Without a translation, a
// language, the plugin, or outside the host, text comes back unchanged. Needs no permission.
func T(text string) string { return X(text, "") }

// X is T with a gettext context (WordPress' _x()), for strings that read the same but translate
// differently ("Post" the noun, "Post" the verb).
func X(text, context string) string {
	if text == "" {
		return text
	}
	resp, err := invokeHost("translate", translateRequest(text, context))
	if err != nil {
		return text
	}
	if v, ok := fastString(resp, translatePrefix, resultSuffix); ok { // fastpath.go
		if v == "" {
			return text
		}
		return v
	}
	raw, err := decodeResponse(resp)
	if err != nil {
		return text
	}
	var r struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Text == "" {
		return text
	}
	return r.Text
}
