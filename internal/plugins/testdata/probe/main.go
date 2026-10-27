// Command probe is a test plugin that exercises the runtime's failure modes,
// permission checks and the host functions beyond hooks. It is built by the
// internal/plugins tests.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"wpgo/sdk/go/wpgo"
)

func init() {
	// Tries to write an option; returns "ok" or the error code.
	wpgo.AddFilter("probe_update_option", 10, func(_ wpgo.Context, v string, _ []any) string {
		if err := wpgo.UpdateOption("probe_option", v); err != nil {
			if wpgo.IsPermissionDenied(err) {
				return "permission_denied"
			}
			return "error: " + err.Error()
		}
		return "ok"
	})
	// Round-trips a value through plugin storage.
	wpgo.AddFilter("probe_storage", 10, func(_ wpgo.Context, v string, _ []any) string {
		if err := wpgo.Storage.Set("k", v); err != nil {
			return "error: " + err.Error()
		}
		got, _, err := wpgo.Storage.Get("k")
		if err != nil {
			return "error: " + err.Error()
		}
		return "stored:" + got
	})
	// Never returns; the runtime must stop it.
	wpgo.AddFilter("probe_loop", 10, func(_ wpgo.Context, v string, _ []any) string {
		n := 0
		for {
			n++
		}
	})
	// Panics; the SDK reports it as a callback failure.
	wpgo.AddFilter("probe_panic", 10, func(_ wpgo.Context, v string, _ []any) string {
		panic("probe panic")
	})
	// Terminates the module; the runtime must re-instantiate it.
	wpgo.AddFilter("probe_exit", 10, func(_ wpgo.Context, v string, _ []any) string {
		os.Exit(3)
		return v
	})
	// Echoes the value with a suffix; used to check recovery.
	wpgo.AddFilter("probe_echo", 10, func(_ wpgo.Context, v string, _ []any) string {
		println("echo called") // goes to the plugin log via stderr
		return v + "!"
	})

	// Shortcode: echoes the tag, sorted attributes and content.
	wpgo.AddShortcode("probe", func(ctx wpgo.Context, attrs map[string]string, content string) string {
		keys := make([]string, 0, len(attrs))
		for k := range attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+"="+attrs[k])
		}
		return fmt.Sprintf("<%s %s|%s>", ctx.Hook, strings.Join(parts, ","), content)
	})
	// A shortcode that is not declared in plugin.json: registration must be refused.
	wpgo.AddShortcode("undeclared", func(wpgo.Context, map[string]string, string) string { return "x" })

	// REST: echoes the request.
	wpgo.RegisterRestRoute("probe/v1", `/echo/(?P<id>\d+)`, []string{"GET", "POST"}, func(_ wpgo.Context, req *wpgo.RestRequest) *wpgo.RestResponse {
		out := map[string]any{"method": req.Method, "id": req.Param("id"), "query": req.Query, "body": req.Body, "route": req.Route}
		if req.User != nil {
			out["user"] = req.User.Login
		}
		resp := wpgo.JSON(200, out)
		resp.Headers = map[string]string{"X-Probe": "1"}
		return resp
	})
	// REST: a namespace that does not belong to the plugin must be refused.
	wpgo.RegisterRestRoute("wp/v2", "/probe", nil, func(wpgo.Context, *wpgo.RestRequest) *wpgo.RestResponse { return wpgo.JSON(200, "no") })

	// Posts: insert, update, get and query; returns a summary.
	wpgo.AddFilter("probe_posts", 10, func(_ wpgo.Context, v string, _ []any) string {
		p, err := wpgo.InsertPost(wpgo.PostInput{Title: wpgo.String(v), Content: wpgo.String("<p>body</p><script>x</script>"), Status: wpgo.String("publish")})
		if err != nil {
			return "insert error: " + err.Error()
		}
		if _, err := wpgo.UpdatePost(p.ID, wpgo.PostInput{Excerpt: wpgo.String("ex"), Meta: map[string]any{"probe": 1}}); err != nil {
			return "update error: " + err.Error()
		}
		got, found, err := wpgo.GetPost(p.ID)
		if err != nil || !found {
			return fmt.Sprintf("get error: %v %v", err, found)
		}
		list, err := wpgo.QueryPosts(wpgo.PostQuery{Search: v, PerPage: 5})
		if err != nil {
			return "query error: " + err.Error()
		}
		return fmt.Sprintf("id=%d title=%s excerpt=%s status=%s meta=%v found=%d total=%d", got.ID, got.Title, got.Excerpt, got.Status, got.Meta["probe"], len(list.Posts), list.Total)
	})
	// Current user.
	wpgo.AddFilter("probe_user", 10, func(_ wpgo.Context, v string, _ []any) string {
		u, err := wpgo.CurrentUser()
		if err != nil {
			return "error: " + err.Error()
		}
		if u == nil {
			return "anonymous"
		}
		return fmt.Sprintf("%s:%v:%v", u.Login, u.Roles, u.Can("edit_posts"))
	})
	// Cron: schedules and unschedules.
	wpgo.AddAction("cron_probe", 10, func(_ wpgo.Context, args []any) {
		raw, _ := json.Marshal(args)
		_ = wpgo.Storage.Set("cron_args", string(raw))
	})
	wpgo.AddFilter("probe_cron", 10, func(_ wpgo.Context, v string, _ []any) string {
		switch v {
		case "schedule":
			if err := wpgo.ScheduleEvent("cron_probe", time.Hour, map[string]any{"n": 1}); err != nil {
				return "error: " + err.Error()
			}
		case "single":
			if err := wpgo.ScheduleSingleEvent("cron_probe", time.Now().Add(time.Hour), nil); err != nil {
				return "error: " + err.Error()
			}
		case "undeclared":
			if err := wpgo.ScheduleEvent("cron_other", time.Hour, nil); err != nil {
				return "error: " + err.Error()
			}
		case "unschedule":
			if err := wpgo.UnscheduleEvent("cron_probe"); err != nil {
				return "error: " + err.Error()
			}
		}
		return "ok"
	})
	// Settings page.
	wpgo.RegisterSettingsPage(wpgo.SettingsPage{PageTitle: "Probe Settings", MenuTitle: "Probe", Fields: []wpgo.SettingsField{
		{ID: "greeting", Label: "Greeting", Type: "text", Default: "hi"},
		{ID: "enabled", Label: "Enabled", Type: "checkbox", Default: true},
		{ID: "level", Label: "Level", Type: "number", Default: 3},
		{ID: "mode", Label: "Mode", Type: "select", Options: []wpgo.SettingsOption{{Value: "a", Label: "A"}, {Value: "b", Label: "B"}}, Default: "a"},
		{ID: "notes", Label: "Notes", Type: "textarea"},
	}})
	wpgo.AddFilter("probe_settings", 10, func(_ wpgo.Context, v string, _ []any) string {
		return wpgo.SettingName("greeting") + "=" + wpgo.SettingString("greeting", "default")
	})
	// Transients.
	wpgo.AddFilter("probe_transient", 10, func(_ wpgo.Context, v string, _ []any) string {
		if err := wpgo.Transients.Set("t", map[string]any{"v": v}, time.Minute); err != nil {
			return "error: " + err.Error()
		}
		var got map[string]any
		ok, err := wpgo.Transients.Get("t", &got)
		if err != nil || !ok {
			return fmt.Sprintf("get error: %v %v", err, ok)
		}
		if err := wpgo.Transients.Delete("t"); err != nil {
			return "delete error: " + err.Error()
		}
		if ok, _ := wpgo.Transients.Get("t", &got); ok {
			return "still there"
		}
		return fmt.Sprint("transient:", got["v"])
	})
	// Assets.
	wpgo.EnqueueStyle(wpgo.Style{Handle: "probe", Src: "probe.css", Where: wpgo.Both})
	wpgo.EnqueueScript(wpgo.Script{Handle: "probe", Src: "js/probe.js", Deps: []string{"missing"}, InFooter: true})
	wpgo.EnqueueScript(wpgo.Script{Handle: "nope", Src: "missing.js"}) // not in the package: refused
}

func main() {}
