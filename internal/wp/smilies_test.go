package wp

import (
	"strings"
	"testing"
)

func TestConvertSmilies(t *testing.T) {
	base := "http://example.test/wp-includes/images/smilies/"
	cases := map[string]string{
		"Hello :) world":                 "Hello \U0001f642 world",
		":)":                             "\U0001f642",
		"a:) b":                          "a:) b", // needs whitespace before
		"so :oops: and :o yes":           "so \U0001f633 and \U0001f62e yes",
		"x&nbsp;:D&nbsp;y":               "x&nbsp;\U0001f600&nbsp;y",
		"line\n;-)\nend":                 "line\n\U0001f609\nend",
		"<code>:)</code> :)":             "<code>:)</code> \U0001f642",
		"<pre class=\"x\">:) </pre> :-(": "<pre class=\"x\">:) </pre> \U0001f641",
		"<a title=\":)\">:)</a>":         "<a title=\":)\">\U0001f642</a>",
		"8) stays":                       "8) stays",
	}
	for in, want := range cases {
		if got := ConvertSmilies(in, base); got != want {
			t.Errorf("ConvertSmilies(%q) = %q, want %q", in, got, want)
		}
	}
	got := ConvertSmilies("green :mrgreen: !", base)
	want := `green <img src="http://example.test/wp-includes/images/smilies/mrgreen.png" alt=":mrgreen:" class="wp-smiley" style="height: 1em; max-height: 1em;" /> !`
	if got != want {
		t.Errorf("mrgreen = %q", got)
	}
}

func TestParseFeed(t *testing.T) {
	rss := `<?xml version="1.0"?><rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:content="http://purl.org/rss/1.0/modules/content/"><channel><title>Site</title>
	<item><title>One &amp; two</title><link>https://example.com/one</link><description><![CDATA[<p>First &amp; best</p>]]></description><pubDate>Mon, 02 Jan 2006 15:04:05 +0000</pubDate><dc:creator><![CDATA[Ann]]></dc:creator></item>
	<item><title>Two</title><link>https://example.com/two</link><content:encoded><![CDATA[Body]]></content:encoded><author>bob@example.com (Bob)</author></item>
	</channel></rss>`
	f, err := ParseFeed([]byte(rss))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "Site" || len(f.Items) != 2 {
		t.Fatalf("feed = %+v", f)
	}
	it := f.Items[0]
	if it.Title != "One & two" || it.Link != "https://example.com/one" || it.Description != "<p>First &amp; best</p>" || it.Author != "Ann" || it.Date.Year() != 2006 {
		t.Errorf("item 0 = %+v", it)
	}
	if f.Items[1].Description != "Body" || !strings.Contains(f.Items[1].Author, "Bob") {
		t.Errorf("item 1 = %+v", f.Items[1])
	}

	atom := `<?xml version="1.0" encoding="utf-8"?><feed xmlns="http://www.w3.org/2005/Atom"><title>Atom site</title>
	<entry><title>Entry</title><link rel="self" href="https://example.com/self"/><link rel="alternate" href="https://example.com/entry"/><summary>Sum</summary><updated>2024-05-06T07:08:09Z</updated><author><name>Cy</name></author></entry></feed>`
	f, err = ParseFeed([]byte(atom))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "Atom site" || len(f.Items) != 1 || f.Items[0].Link != "https://example.com/entry" || f.Items[0].Author != "Cy" || f.Items[0].Description != "Sum" || f.Items[0].Date.Day() != 6 {
		t.Errorf("atom = %+v", f)
	}

	latin := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss version=\"2.0\"><channel><title>Caf\xe9</title><item><title>\xe9</title></item></channel></rss>"
	f, err = ParseFeed([]byte(latin))
	if err != nil || f.Title != "Café" || f.Items[0].Title != "é" {
		t.Errorf("latin-1 feed = %+v, %v", f, err)
	}

	if _, err := ParseFeed([]byte("<html><body>nope</body></html>")); err == nil {
		t.Error("HTML accepted as a feed")
	}
}
