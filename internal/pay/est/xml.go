package est

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strconv"
	"strings"
)

// kv is one element of a CC5Request: name, text value.
type kv = [2]string

// buildXML writes a CC5Request. Characters outside printable ASCII are written as character
// references, so the body is valid whatever encoding the bank assumes (as better-payment does).
// extra, when not nil, becomes the <Extra> element.
func buildXML(fields, extra []kv) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="ISO-8859-9"?><CC5Request>`)
	writeKV(&b, fields)
	if extra != nil {
		b.WriteString("<Extra>")
		writeKV(&b, extra)
		b.WriteString("</Extra>")
	}
	b.WriteString(`</CC5Request>`)
	return b.Bytes()
}

func writeKV(b *bytes.Buffer, fields []kv) {
	for _, f := range fields {
		b.WriteString("<" + f[0] + ">" + escapeXML(f[1]) + "</" + f[0] + ">")
	}
}

func escapeXML(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString("&quot;")
		case r == '\'':
			b.WriteString("&#39;")
		case r < 0x20 || r > 0x7e:
			b.WriteString("&#" + strconv.Itoa(int(r)) + ";")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// response is a parsed CC5Response.
type response struct {
	OrderID        string            `xml:"OrderId"`
	Response       string            `xml:"Response"`
	AuthCode       string            `xml:"AuthCode"`
	HostRefNum     string            `xml:"HostRefNum"`
	ProcReturnCode string            `xml:"ProcReturnCode"`
	TransID        string            `xml:"TransId"`
	ErrMsg         string            `xml:"ErrMsg"`
	Extra          map[string]string `xml:"-"`
}

func (r *response) approved() bool { return r.ProcReturnCode == "00" && r.Response == "Approved" }

// parseResponse reads a CC5Response with encoding/xml, which resolves no external entities. The
// bank declares ISO-8859-9; its answers are read as bytes (CharsetReader passes them through), which
// keeps the ASCII fields this package uses exact.
func parseResponse(body []byte) (*response, error) {
	var raw struct {
		XMLName xml.Name `xml:"CC5Response"`
		response
		Extra struct {
			Items []struct {
				XMLName xml.Name
				Value   string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"Extra"`
	}
	d := xml.NewDecoder(bytes.NewReader(body))
	d.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	if err := d.Decode(&raw); err != nil {
		return nil, errors.New("pay: EST returned an unexpected response")
	}
	r := raw.response
	r.Extra = map[string]string{}
	for _, it := range raw.Extra.Items {
		if _, ok := r.Extra[it.XMLName.Local]; !ok {
			r.Extra[it.XMLName.Local] = strings.TrimSpace(it.Value)
		}
	}
	for _, p := range []*string{&r.OrderID, &r.Response, &r.AuthCode, &r.HostRefNum, &r.ProcReturnCode, &r.TransID, &r.ErrMsg} {
		*p = strings.TrimSpace(*p)
	}
	return &r, nil
}
