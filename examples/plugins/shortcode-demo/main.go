// Command shortcode-demo is a wpgo example plugin that shows the SDK features
// beyond hooks: a shortcode, REST routes, a settings page, package assets,
// transients, posts and scheduled events.
//
//	[gopher-facts count="3"]   lists random gopher facts (styled by assets/style.css)
//	[recent-gophers]           lists the newest posts through the posts API
//	GET /wp-json/shortcode-demo/v1/facts            all facts as JSON
//	GET /wp-json/shortcode-demo/v1/facts/<id>       one fact
//	POST /wp-json/shortcode-demo/v1/facts           adds a fact (editors only)
//	Settings › Gopher Facts                          heading, count and a greeting toggle
//
// Build: GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
package main

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wpgo/sdk/go/wpgo"
)

// facts are the built-in facts; facts added through the REST route live in plugin storage.
var facts = []string{
	"Gophers can dig a tunnel of 60 cm per hour, which is slower than Go compiles.",
	"The Go gopher was drawn by Renée French and has no official name.",
	"A group of gophers is called a tunnel, at least in this plugin.",
	"Goroutines are cheaper than gopher burrows: a few kilobytes each.",
	"Gophers never block: they always leave a channel open.",
}

const cronHook = "cron_shortcode_demo_tick"

func init() {
	wpgo.RegisterSettingsPage(wpgo.SettingsPage{
		PageTitle:   "Gopher Facts",
		MenuTitle:   "Gopher Facts",
		Description: "Options of the shortcode-demo example plugin.",
		Fields: []wpgo.SettingsField{
			{ID: "heading", Label: "Heading", Type: "text", Default: "Did you know?", Placeholder: "Shown above the facts"},
			{ID: "count", Label: "Facts per shortcode", Type: "number", Default: 3, Min: ptr(1), Max: ptr(10)},
			{ID: "greet", Label: "Greet logged-in readers", Type: "checkbox", Default: true, Description: "Adds a personal greeting above the list."},
			{ID: "style", Label: "List style", Type: "select", Default: "cards", Options: []wpgo.SettingsOption{{Value: "cards", Label: "Cards"}, {Value: "plain", Label: "Plain list"}}},
			{ID: "footer", Label: "Footer note", Type: "textarea", Rows: 3, Description: "Plain text printed under the facts."},
		},
	})
	wpgo.EnqueueStyle(wpgo.Style{Handle: "gopher-facts", Src: "style.css", Where: wpgo.Front})

	wpgo.AddShortcode("gopher-facts", gopherFacts)
	wpgo.AddShortcode("recent-gophers", recentPosts)

	wpgo.RegisterRestRoute("shortcode-demo/v1", "/facts", []string{"GET", "POST"}, factsRoute)
	wpgo.RegisterRestRoute("shortcode-demo/v1", `/facts/(?P<id>\d+)`, []string{"GET"}, factRoute)

	// A scheduled event that counts its runs in plugin storage.
	wpgo.AddAction(cronHook, wpgo.DefaultPriority, func(wpgo.Context, []any) {
		n := 0
		if v, ok, _ := wpgo.Storage.Get("ticks"); ok {
			n, _ = strconv.Atoi(v)
		}
		_ = wpgo.Storage.Set("ticks", strconv.Itoa(n+1))
	})
	wpgo.OnInit(func() {
		if err := wpgo.ScheduleEvent(cronHook, time.Hour, nil); err != nil {
			wpgo.Log("warning", "schedule: "+err.Error())
		}
	})
}

func ptr(f float64) *float64 { return &f }

// allFacts returns the built-in facts plus those added through the REST route, cached in a
// transient for a minute.
func allFacts() []string {
	var cached []string
	if ok, _ := wpgo.Transients.Get("facts", &cached); ok {
		return cached
	}
	out := append([]string(nil), facts...)
	if extra, ok, _ := wpgo.Storage.Get("extra_facts"); ok && extra != "" {
		out = append(out, strings.Split(extra, "\n")...)
	}
	_ = wpgo.Transients.Set("facts", out, time.Minute)
	return out
}

func gopherFacts(_ wpgo.Context, attrs map[string]string, _ string) string {
	count := int(wpgo.SettingFloat("count", 3))
	if n, err := strconv.Atoi(attrs["count"]); err == nil && n > 0 {
		count = n
	}
	list := allFacts()
	if count > len(list) {
		count = len(list)
	}
	var sb strings.Builder
	sb.WriteString(`<div class="gopher-facts gopher-facts--` + html.EscapeString(wpgo.SettingString("style", "cards")) + `">`)
	if wpgo.SettingBool("greet", true) {
		if u, _ := wpgo.CurrentUser(); u != nil {
			sb.WriteString(`<p class="gopher-facts__greeting">Hello, ` + html.EscapeString(u.DisplayName) + `!</p>`)
		}
	}
	sb.WriteString(`<h3>` + html.EscapeString(wpgo.SettingString("heading", "Did you know?")) + `</h3><ul>`)
	for _, f := range list[:count] {
		sb.WriteString("<li>" + html.EscapeString(f) + "</li>")
	}
	sb.WriteString("</ul>")
	if note := wpgo.SettingString("footer", ""); note != "" {
		sb.WriteString(`<p class="gopher-facts__footer">` + html.EscapeString(note) + `</p>`)
	}
	sb.WriteString("</div>")
	return sb.String()
}

func recentPosts(_ wpgo.Context, attrs map[string]string, _ string) string {
	n, _ := strconv.Atoi(attrs["count"])
	if n <= 0 {
		n = 5
	}
	list, err := wpgo.QueryPosts(wpgo.PostQuery{PerPage: n})
	if err != nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(`<ul class="recent-gophers">`)
	for _, p := range list.Posts {
		sb.WriteString(`<li><a href="` + html.EscapeString(p.Link) + `">` + html.EscapeString(p.Title) + `</a></li>`)
	}
	sb.WriteString("</ul>")
	return sb.String()
}

func factsRoute(_ wpgo.Context, req *wpgo.RestRequest) *wpgo.RestResponse {
	if req.Method == http.MethodGet {
		return wpgo.JSON(http.StatusOK, map[string]any{"facts": allFacts()})
	}
	if !req.User.Can("edit_posts") {
		return wpgo.RestError(http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to add facts.")
	}
	var in struct {
		Fact string `json:"fact"`
	}
	if err := req.BindJSON(&in); err != nil || strings.TrimSpace(in.Fact) == "" {
		return wpgo.RestError(http.StatusBadRequest, "rest_invalid_param", "fact is required")
	}
	extra, _, _ := wpgo.Storage.Get("extra_facts")
	if extra != "" {
		extra += "\n"
	}
	_ = wpgo.Storage.Set("extra_facts", extra+strings.TrimSpace(in.Fact))
	_ = wpgo.Transients.Delete("facts")
	return wpgo.JSON(http.StatusCreated, map[string]any{"id": len(allFacts()), "fact": in.Fact})
}

func factRoute(_ wpgo.Context, req *wpgo.RestRequest) *wpgo.RestResponse {
	id, _ := strconv.Atoi(req.Param("id"))
	list := allFacts()
	if id < 1 || id > len(list) {
		return wpgo.RestError(http.StatusNotFound, "rest_not_found", fmt.Sprintf("No fact %d.", id))
	}
	return wpgo.JSON(http.StatusOK, map[string]any{"id": id, "fact": list[id-1]})
}

func main() {}
