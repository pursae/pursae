package wp

import (
	"html"
	"regexp"
	"sort"
	"strings"
)

// smilies is $wpsmiliestrans from smilies_init(): every smiley maps to an emoji character
// except :mrgreen:, which stays the classic image (translate_smiley() only prints <img> for image files).
var smilies = map[string]string{
	":mrgreen:": "mrgreen.png",
	":neutral:": "\U0001f610",
	":twisted:": "\U0001f608",
	":arrow:":   "➡",
	":shock:":   "\U0001f62f",
	":smile:":   "\U0001f642",
	":???:":     "\U0001f615",
	":cool:":    "\U0001f60e",
	":evil:":    "\U0001f47f",
	":grin:":    "\U0001f600",
	":idea:":    "\U0001f4a1",
	":oops:":    "\U0001f633",
	":razz:":    "\U0001f61b",
	":roll:":    "\U0001f644",
	":wink:":    "\U0001f609",
	":cry:":     "\U0001f625",
	":eek:":     "\U0001f62e",
	":lol:":     "\U0001f606",
	":mad:":     "\U0001f621",
	":sad:":     "\U0001f641",
	"8-)":       "\U0001f60e",
	"8-O":       "\U0001f62f",
	":-(":       "\U0001f641",
	":-)":       "\U0001f642",
	":-?":       "\U0001f615",
	":-D":       "\U0001f600",
	":-P":       "\U0001f61b",
	":-o":       "\U0001f62e",
	":-x":       "\U0001f621",
	":-|":       "\U0001f610",
	";-)":       "\U0001f609",
	// '8)' is left out on purpose: it breaks regular text with frequency (smilies_init()).
	"8O":  "\U0001f62f",
	":(":  "\U0001f641",
	":)":  "\U0001f642",
	":?":  "\U0001f615",
	":D":  "\U0001f600",
	":P":  "\U0001f61b",
	":o":  "\U0001f62e",
	":x":  "\U0001f621",
	":|":  "\U0001f610",
	";)":  "\U0001f609",
	":!:": "❗",
	":?:": "❓",
}

// smileyKeys is the table sorted longest first, so ":oops:" wins over ":o" at the same position.
var smileyKeys = func() []string {
	keys := make([]string, 0, len(smilies))
	for k := range smilies {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}()

var (
	smileyTagRe    = regexp.MustCompile(`<[^>]*>`)
	smileyIgnoreRe = regexp.MustCompile(`^<(code|pre|style|script|textarea)[^>]*>`)
)

// smileySpaceBefore reports whether position i follows wp_spaces_regexp() (a space, newline, tab,
// no-break space or &nbsp;) or the start of the text.
func smileySpaceBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	switch s[i-1] {
	case ' ', '\n', '\r', '\t':
		return true
	}
	return strings.HasSuffix(s[:i], " ") || strings.HasSuffix(s[:i], "&nbsp;")
}

func smileySpaceAfter(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	switch s[i] {
	case ' ', '\n', '\r', '\t':
		return true
	}
	return strings.HasPrefix(s[i:], " ") || strings.HasPrefix(s[i:], "&nbsp;")
}

// translateSmiley is translate_smiley(): emoji smilies are returned as the character, image smilies
// as the wp-smiley <img>. imgBase is includes_url('images/smilies/').
func translateSmiley(smiley, imgBase string) string {
	img := smilies[smiley]
	ext := ""
	if i := strings.LastIndex(img, "."); i >= 0 {
		ext = strings.ToLower(img[i+1:])
	}
	switch ext {
	case "jpg", "jpeg", "jpe", "gif", "png", "webp", "avif":
		return `<img src="` + html.EscapeString(imgBase+img) + `" alt="` + html.EscapeString(smiley) + `" class="wp-smiley" style="height: 1em; max-height: 1em;" />`
	}
	return img
}

func convertSmileyText(text, imgBase string) string {
	var sb strings.Builder
	i := 0
	for i < len(text) {
		if smileySpaceBefore(text, i) {
			matched := false
			for _, k := range smileyKeys {
				if strings.HasPrefix(text[i:], k) && smileySpaceAfter(text, i+len(k)) {
					sb.WriteString(translateSmiley(k, imgBase))
					i += len(k)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}
		sb.WriteByte(text[i])
		i++
	}
	return sb.String()
}

// ConvertSmilies is convert_smilies(): smilies surrounded by whitespace become emoji characters
// (or the wp-smiley image), leaving tags and the contents of <code>, <pre>, <style>, <script> and
// <textarea> alone. The caller checks the use_smilies option.
func ConvertSmilies(text, imgBase string) string {
	if text == "" {
		return text
	}
	var sb strings.Builder
	ignore := ""
	last := 0
	emit := func(chunk string) {
		if chunk == "" {
			return
		}
		if chunk[0] == '<' {
			if ignore == "" {
				if m := smileyIgnoreRe.FindStringSubmatch(chunk); m != nil {
					ignore = m[1]
				}
			} else if chunk == "</"+ignore+">" {
				ignore = ""
			}
			sb.WriteString(chunk)
			return
		}
		if ignore != "" {
			sb.WriteString(chunk)
			return
		}
		sb.WriteString(convertSmileyText(chunk, imgBase))
	}
	for _, loc := range smileyTagRe.FindAllStringIndex(text, -1) {
		emit(text[last:loc[0]])
		emit(text[loc[0]:loc[1]])
		last = loc[1]
	}
	emit(text[last:])
	return sb.String()
}
