package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wpgo/internal/cli"
	"wpgo/internal/settings"
	"wpgo/internal/store"
)

// registerOptionCLI registers the options and site commands. See cli.go for the environment (cliEnv) and conventions.
//
// Nouns: option, transient, cache, rewrite, maintenance-mode, search-replace.
//
// Options are rows of the settings table (JSONB values, no PHP serialization). Writes go through
// settings.Store.Set, whose NOTIFY trigger makes running servers reload their settings cache, and
// end with a page cache purge-all announced on wpgo_cache_purge, as the Settings screens do.
func registerOptionCLI(r *cli.Registry, e *cliEnv) {
	registerOptionCommands(r, e)
	registerTransientCLI(r, e)
	registerCacheCLI(r, e)
	registerRewriteCLI(r, e)
	registerMaintenanceCLI(r, e)
	registerSearchReplaceCLI(r, e)
}

// optionProtected are options `option delete` refuses: siteurl is the install marker (Installed),
// auth_secret is the key nonces, Sign() tokens and the sealed secrets derive from (without PURSAE_SECRET).
var optionProtected = map[string]string{
	"siteurl":     "it marks the site as installed; deleting it would reopen the installer",
	"auth_secret": "it signs nonces and links and encrypts stored passwords and API keys; deleting it would make them unreadable",
}

func registerOptionCommands(r *cli.Registry, e *cliEnv) {
	r.Noun("option", "Retrieve and set site options (the settings table).")

	r.Add(cli.Command{
		Path:  "option get",
		Short: "Get the value of an option.",
		Long: "Prints the stored value: scalars as plain text, objects and arrays as JSON (or YAML with --format=yaml).\n" +
			"When the option has no row but is a core option, its built-in default is printed and a note goes to stderr.",
		Args:          []cli.Arg{{Name: "key", Usage: "The option name."}},
		Formats:       []string{"plaintext"},
		DefaultFormat: "plaintext",
		Examples:      []string{"option get blogname", "option get maintenance_settings --format=json"},
		Run: func(c *cli.Ctx) error {
			pool, err := e.Pool(c.Context)
			if err != nil {
				return err
			}
			key := c.Arg(0)
			v, _, found, err := optionRow(c.Context, pool, key)
			if err != nil {
				return err
			}
			if !found {
				def, ok := settings.Defaults[key]
				if !ok {
					return cli.Errorf("Could not get '%s' option. Does it exist?", key)
				}
				if !c.Quiet {
					fmt.Fprintf(c.Stderr(), "(default) '%s' has no stored value; showing the built-in default.\n", key)
				}
				v = def
			}
			return c.PrintValue(v)
		},
	})

	r.Add(cli.Command{
		Path:  "option list",
		Short: "List options and their values.",
		Long: "Values are truncated in table output. Values whose key looks secret (password, token, secret, api key …)\n" +
			"are masked unless --unmask is given.",
		Flags: []cli.Flag{
			{Name: "search", Usage: "Only options whose name matches this glob (* and ? wildcards)."},
			{Name: "autoload", Usage: "Only autoloaded (on) or not autoloaded (off) options.", Values: []string{"on", "off", "yes", "no"}},
			{Name: "defaults", Bool: true, Usage: "Also list core options that have no row, with their built-in default."},
			{Name: "unmask", Bool: true, Usage: "Print secret-looking values instead of ********."},
			{Name: "orderby", Usage: "Sort field.", Default: "option_name", Values: []string{"option_name", "updated_at"}},
			{Name: "order", Usage: "Sort order.", Default: "asc", Values: []string{"asc", "desc", "ASC", "DESC"}},
		},
		Examples: []string{"option list --search='wpgo_cache_*'", "option list --autoload=off --format=json", "option list --defaults --fields=option_name,source"},
		Run: func(c *cli.Ctx) error {
			pool, err := e.Pool(c.Context)
			if err != nil {
				return err
			}
			q := `SELECT key, value, autoload, grp, updated_at FROM settings WHERE ($1 = '' OR key LIKE $1 ESCAPE '\')`
			args := []any{optGlobToLike(c.Flag("search"))}
			if c.Has("autoload") {
				on, _ := cli.ParseBool(c.Flag("autoload"))
				q += ` AND autoload = $2`
				args = append(args, on)
			}
			rows, err := pool.Query(c.Context, q, args...)
			if err != nil {
				return err
			}
			var out []cli.Row
			seen := map[string]bool{}
			for rows.Next() {
				var key, grp string
				var raw []byte
				var autoload bool
				var updated time.Time
				if err := rows.Scan(&key, &raw, &autoload, &grp, &updated); err != nil {
					rows.Close()
					return err
				}
				var v any
				_ = json.Unmarshal(raw, &v)
				seen[key] = true
				out = append(out, cli.Row{"option_name": key, "option_value": v, "autoload": map[bool]string{true: "on", false: "off"}[autoload],
					"grp": grp, "updated_at": updated, "source": "row", "size_bytes": len(raw)})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if c.Bool("defaults") && (!c.Has("autoload") || c.Flag("autoload") == "on" || c.Flag("autoload") == "yes") {
				pattern := c.Flag("search")
				for k, v := range settings.Defaults {
					if seen[k] || (pattern != "" && !optGlobMatch(pattern, k)) {
						continue
					}
					raw, _ := json.Marshal(v)
					out = append(out, cli.Row{"option_name": k, "option_value": v, "autoload": "on", "grp": "", "updated_at": nil, "source": "default", "size_bytes": len(raw)})
				}
			}
			desc := strings.EqualFold(c.Flag("order"), "desc")
			byUpdated := c.Flag("orderby") == "updated_at"
			sort.SliceStable(out, func(i, j int) bool {
				a, b := out[i], out[j]
				var less bool
				if byUpdated {
					ta, _ := a["updated_at"].(time.Time)
					tb, _ := b["updated_at"].(time.Time)
					less = ta.Before(tb)
					if ta.Equal(tb) {
						less = a["option_name"].(string) < b["option_name"].(string)
					}
				} else {
					less = a["option_name"].(string) < b["option_name"].(string)
				}
				if desc {
					return !less
				}
				return less
			})
			for _, row := range out {
				if !c.Bool("unmask") {
					row["option_value"] = maskSecrets(row["option_value"], row["option_name"].(string))
				}
				if c.Format == "table" && c.Field == "" {
					row["option_value"] = optTruncate(cli.Stringify(row["option_value"]), 80)
				}
			}
			return c.Print(out, cli.View{Fields: []string{"option_name", "option_value", "autoload"}, ID: "option_name"})
		},
	})

	writeFlags := []cli.Flag{
		{Name: "autoload", Usage: "Whether running servers load the option into memory (default: unchanged, new rows: yes).", Values: []string{"yes", "no", "on", "off"}},
	}
	writeLong := "The value is read from STDIN when omitted. With --format=json it is parsed as JSON. Otherwise it is\n" +
		"converted to the type of the core option (bool, int, float, string), or of the stored value; a value\n" +
		"for an unknown option is stored as a string unless it is a JSON object or array.\n\n" +
		"Core options are checked like the Settings screens do: siteurl/home must be http(s) URLs (the trailing\n" +
		"slash is dropped), timezone_string must be a known zone or UTC+N (stored as gmt_offset), posts_per_page\n" +
		"is at least 1, permalink_structure is normalized, and so on. Running servers pick the value up at once;\n" +
		"the page cache is purged on every instance."

	update := func(c *cli.Ctx, add bool) error {
		ctx := c.Context
		pool, err := e.Pool(ctx)
		if err != nil {
			return err
		}
		set, err := e.Settings(ctx)
		if err != nil {
			return err
		}
		key := c.Arg(0)
		if strings.TrimSpace(key) == "" {
			return cli.Usagef("the option name is empty")
		}
		raw, err := optionInput(c, 1)
		if err != nil {
			return err
		}
		cur, _, exists, err := optionRow(ctx, pool, key)
		if err != nil {
			return err
		}
		if add && exists {
			return cli.Errorf("Could not add option '%s'. Does it already exist?", key)
		}
		v, err := optionCoerce(key, raw, c.Format == "json", cur, exists)
		if err != nil {
			return err
		}
		values, err := optionNormalize(key, v)
		if err != nil {
			return err
		}
		if !add && exists && len(values) == 1 && optJSONEqual(cur, values[key]) && !c.Has("autoload") {
			c.Success("Value passed for '%s' option is unchanged.", key)
			return nil
		}
		if err := set.Set(ctx, values); err != nil {
			return err
		}
		if c.Has("autoload") {
			on, _ := cli.ParseBool(c.Flag("autoload"))
			if _, err := pool.Exec(ctx, `UPDATE settings SET autoload = $2 WHERE key = $1`, key, on); err != nil {
				return err
			}
			if !on {
				c.Warning("'%s' is not autoloaded now: running servers read its default instead of the stored value.", key)
			}
		}
		if key == "WPLANG" {
			c.Warning("the language pack is not downloaded by this command; install it with the language commands or from Settings › General.")
		}
		if optionNeedsRestart(key) {
			c.Warning("'%s' is applied by the server process at start: restart the server to apply it fully.", key)
		}
		e.optionAudit(c, key, "Option '"+key+"' "+map[bool]string{true: "added", false: "updated"}[add]+" from the command line.")
		if err := e.PurgeAll(ctx); err != nil {
			c.Warning("page cache purge: %v", err)
		}
		if add {
			c.Success("Added '%s' option.", key)
		} else {
			c.Success("Updated '%s' option.", key)
		}
		return nil
	}

	r.Add(cli.Command{
		Path:          "option update",
		Short:         "Update an option value (creates it when missing).",
		Long:          writeLong,
		Args:          []cli.Arg{{Name: "key", Usage: "The option name."}, {Name: "value", Usage: "The new value (STDIN when omitted).", Optional: true}},
		Flags:         writeFlags,
		Formats:       []string{"plaintext"},
		DefaultFormat: "plaintext",
		Examples: []string{
			"option update blogname 'My Site'",
			"option update posts_per_page 5",
			"option update home https://example.com",
			"option update timezone_string Europe/Istanbul",
			`option update my_plugin_settings '{"a":1}' --format=json`,
			"echo 'Just another site' | option update blogdescription",
		},
		Run: func(c *cli.Ctx) error { return update(c, false) },
	})

	r.Add(cli.Command{
		Path:          "option add",
		Short:         "Add an option; fails when it already exists.",
		Long:          writeLong,
		Args:          []cli.Arg{{Name: "key", Usage: "The option name."}, {Name: "value", Usage: "The value (STDIN when omitted).", Optional: true}},
		Flags:         writeFlags,
		Formats:       []string{"plaintext"},
		DefaultFormat: "plaintext",
		Examples:      []string{"option add my_option foobar", `option add my_list '["a","b"]' --format=json --autoload=no`},
		Run:           func(c *cli.Ctx) error { return update(c, true) },
	})

	r.Add(cli.Command{
		Path:  "option delete",
		Short: "Delete one or more options (core options fall back to their defaults).",
		Long: "Refuses siteurl (the install marker) and auth_secret (signs cookies and nonces). Running servers\n" +
			"reload their settings at once and use the built-in default of a deleted core option.",
		Args:     []cli.Arg{{Name: "key", Repeat: true, Usage: "Option names."}},
		Examples: []string{"option delete my_option --yes", "option delete old_a old_b --yes"},
		Run: func(c *cli.Ctx) error {
			ctx := c.Context
			keys := optUnique(c.Args)
			for _, k := range keys {
				if why, ok := optionProtected[k]; ok {
					return cli.Errorf("refusing to delete '%s': %s.", k, why)
				}
			}
			if optContains(keys, "home") {
				c.Warning("deleting 'home' makes links fall back to PURSAE_SITE_URL.")
			}
			if err := c.Confirm("Delete %d option(s): %s?", len(keys), strings.Join(keys, ", ")); err != nil {
				return err
			}
			pool, err := e.Pool(ctx)
			if err != nil {
				return err
			}
			rows, err := pool.Query(ctx, `DELETE FROM settings WHERE key = ANY($1) RETURNING key`, keys)
			if err != nil {
				return err
			}
			deleted := map[string]bool{}
			for rows.Next() {
				var k string
				if err := rows.Scan(&k); err != nil {
					rows.Close()
					return err
				}
				deleted[k] = true
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			var failed int
			for _, k := range keys {
				if deleted[k] {
					c.Success("Deleted '%s' option.", k)
				} else {
					c.Warning("Could not delete '%s' option. Does it exist?", k)
					failed++
				}
			}
			if len(deleted) > 0 {
				if set, err := e.Settings(ctx); err == nil {
					_ = set.Load(ctx)
				}
				for k := range deleted {
					if optionNeedsRestart(k) {
						c.Warning("'%s' is applied by the server process at start: restart the server to apply it fully.", k)
					}
				}
				e.optionAudit(c, strings.Join(keys, ","), "Option(s) deleted from the command line: "+strings.Join(keys, ", ")+".")
				if err := e.PurgeAll(ctx); err != nil {
					c.Warning("page cache purge: %v", err)
				}
			}
			if failed > 0 {
				return cli.Exit(cli.ExitFailure)
			}
			return nil
		},
	})

	r.Add(cli.Command{
		Path:  "option pluck",
		Short: "Get a nested value from an object option.",
		Long:  "Walks the JSON value of <key> along the path: object keys by name, array items by index.",
		Args: []cli.Arg{{Name: "key", Usage: "The option name."},
			{Name: "key-path", Repeat: true, Usage: "Keys (or array indexes) to walk."}},
		Formats:       []string{"plaintext"},
		DefaultFormat: "plaintext",
		Examples:      []string{"option pluck maintenance_settings status", "option pluck adminsecgo_settings hide_login"},
		Run: func(c *cli.Ctx) error {
			v, err := e.optionValue(c.Context, c.Arg(0))
			if err != nil {
				return err
			}
			got, err := jsonPathGet(v, c.Args[1:])
			if err != nil {
				return cli.Errorf("%v", err)
			}
			return c.PrintValue(got)
		},
	})

	r.Add(cli.Command{
		Path:  "option patch",
		Short: "Insert, update or delete a nested value of an object option.",
		Long: "insert adds a key that must not exist yet, update changes one that must exist, delete removes it.\n" +
			"For insert and update the last argument is the value (STDIN when only the path is given and STDIN is\n" +
			"not a terminal). With --format=json it is parsed as JSON; otherwise update keeps the type of the\n" +
			"current value (bool, number) and insert stores a string.",
		Args: []cli.Arg{{Name: "action", Usage: "insert, update or delete."}, {Name: "key", Usage: "The option name."},
			{Name: "key-path", Repeat: true, Usage: "Keys (or array indexes), then the value for insert/update."}},
		Formats:       []string{"plaintext"},
		DefaultFormat: "plaintext",
		Examples: []string{
			"option patch update maintenance_settings mode coming_soon",
			"option patch update maintenance_settings status true",
			`option patch insert my_settings colors '["red","blue"]' --format=json`,
			"option patch delete my_settings old_key --yes",
		},
		Run: func(c *cli.Ctx) error {
			ctx := c.Context
			action, key := c.Arg(0), c.Arg(1)
			rest := c.Args[2:]
			switch action {
			case "insert", "update", "delete":
			default:
				return cli.Usagef("invalid action %q (insert, update or delete)", action)
			}
			var raw string
			path := rest
			if action != "delete" {
				switch {
				case len(rest) >= 2:
					path, raw = rest[:len(rest)-1], rest[len(rest)-1]
				case len(rest) == 1 && !c.Interactive():
					b, err := io.ReadAll(c.Stdin())
					if err != nil {
						return err
					}
					raw = strings.TrimRight(string(b), "\r\n")
				default:
					return cli.Usagef("%s needs a key path and a value", action)
				}
			}
			if len(path) == 0 {
				return cli.Usagef("missing key path")
			}
			v, err := e.optionValue(ctx, key)
			if err != nil {
				return err
			}
			var nv any
			if action != "delete" {
				cur, _ := jsonPathGet(v, path)
				nv, err = patchValue(raw, c.Format == "json", cur)
				if err != nil {
					return err
				}
			} else if err := c.Confirm("Delete '%s' from option '%s'?", strings.Join(path, "."), key); err != nil {
				return err
			}
			out, err := jsonPathPatch(v, action, path, nv)
			if err != nil {
				return cli.Errorf("%v", err)
			}
			set, err := e.Settings(ctx)
			if err != nil {
				return err
			}
			if err := set.Set(ctx, map[string]any{key: out}); err != nil {
				return err
			}
			if optionNeedsRestart(key) {
				c.Warning("'%s' is applied by the server process at start: restart the server to apply it fully.", key)
			}
			e.optionAudit(c, key, "Option '"+key+"' patched ("+action+" "+strings.Join(path, ".")+") from the command line.")
			if err := e.PurgeAll(ctx); err != nil {
				c.Warning("page cache purge: %v", err)
			}
			c.Success("Updated '%s' option.", key)
			return nil
		},
	})
}

// --- option helpers -------------------------------------------------------------------

// optionRow reads one settings row, including options that are not autoloaded.
func optionRow(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key string) (v any, autoload, found bool, err error) {
	var raw []byte
	err = q.QueryRow(ctx, `SELECT value, autoload FROM settings WHERE key = $1`, key).Scan(&raw, &autoload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, false, fmt.Errorf("option %s: %w", key, err)
	}
	return v, autoload, true, nil
}

// optionValue is the stored value of key, else its core default, else an error.
func (e *cliEnv) optionValue(ctx context.Context, key string) (any, error) {
	pool, err := e.Pool(ctx)
	if err != nil {
		return nil, err
	}
	v, _, found, err := optionRow(ctx, pool, key)
	if err != nil {
		return nil, err
	}
	if !found {
		def, ok := settings.Defaults[key]
		if !ok {
			return nil, cli.Errorf("Could not get '%s' option. Does it exist?", key)
		}
		// Round-trip through JSON so defaults have the types of loaded values.
		b, _ := json.Marshal(def)
		_ = json.Unmarshal(b, &v)
	}
	return v, nil
}

// optionInput is positional argument i, or STDIN (trailing newline dropped) when it is absent.
func optionInput(c *cli.Ctx, i int) (string, error) {
	if i < len(c.Args) {
		return c.Args[i], nil
	}
	if c.Interactive() {
		return "", cli.Usagef("missing argument <value> (or pipe it on STDIN)")
	}
	b, err := io.ReadAll(c.Stdin())
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// optionAudit writes a settings_updated row to the Pursae Security log, as optionsSave does (only
// while the plugin is active).
func (e *cliEnv) optionAudit(c *cli.Ctx, object, msg string) {
	s, err := e.Server(c.Context)
	if err != nil || !s.adminsecOn() {
		return
	}
	ev := asEvent{Event: "settings_updated", Severity: "notice", Object: object, Message: msg, Username: "cli"}
	if c.Flag("user") != "" {
		if u, err := e.Actor(c); err == nil {
			ev.User = u
		}
	}
	s.asAudit(c.Context, ev)
}

// optionCoerce turns a command-line value into the value stored for key. asJSON parses JSON. Else
// the type follows the core default of key, then the type of the current value; an unknown option
// is a string, unless the value is a JSON object or array.
func optionCoerce(key, raw string, asJSON bool, cur any, exists bool) (any, error) {
	if asJSON {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, cli.Errorf("invalid JSON value for '%s': %v", key, err)
		}
		return v, nil
	}
	typ, ok := settings.Defaults[key]
	if !ok && exists {
		typ = cur
	}
	switch typ.(type) {
	case bool:
		b, err := cli.ParseBool(raw)
		if err != nil {
			return nil, cli.Errorf("'%s' is a boolean option: use true/false, 1/0, yes/no or on/off", key)
		}
		return b, nil
	case int:
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, cli.Errorf("'%s' is an integer option, got %q", key, raw)
		}
		return n, nil
	case float64:
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			if ok { // a core float option (gmt_offset)
				return nil, cli.Errorf("'%s' is a number option, got %q", key, raw)
			}
			return raw, nil // a stored number turned into text by the caller's choice
		}
		if !ok && f == float64(int64(f)) && !strings.ContainsAny(raw, ".eE") {
			return int64(f), nil
		}
		return f, nil
	case map[string]any, []any:
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, cli.Errorf("'%s' holds a JSON object or array: pass JSON (--format=json): %v", key, err)
		}
		return v, nil
	}
	if t := strings.TrimSpace(raw); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		var v any
		if json.Unmarshal([]byte(t), &v) == nil {
			return v, nil
		}
	}
	return raw, nil
}

// optionNormalize applies the Settings screens' per-key rules (optionsSave, admin_options.go) to one
// value and returns the values to store: a manual UTC offset in timezone_string becomes gmt_offset.
func optionNormalize(key string, v any) (map[string]any, error) {
	out := map[string]any{key: v}
	s, isStr := v.(string)
	if isStr {
		s = strings.TrimSpace(s)
	}
	if _, isInt := settings.Defaults[key].(int); isInt {
		if n, ok := v.(int); ok && n < 0 {
			out[key] = 0
		}
	}
	switch key {
	case "siteurl", "home":
		if !isStr {
			return nil, cli.Errorf("'%s' must be a URL", key)
		}
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, cli.Errorf("'%s' must be an http(s) URL, got %q", key, s)
		}
		out[key] = strings.TrimRight(s, "/")
	case "timezone_string":
		if !isStr {
			return nil, cli.Errorf("'%s' must be a time zone name", key)
		}
		if strings.HasPrefix(s, "UTC") && s != "UTC" {
			off, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimPrefix(s, "UTC"), "+"), 64)
			if err != nil {
				return nil, cli.Errorf("invalid UTC offset %q (e.g. UTC+3, UTC-5.5)", s)
			}
			out[key], out["gmt_offset"] = "", off
		} else if _, err := time.LoadLocation(s); err != nil {
			return nil, cli.Errorf("unknown time zone %q", s)
		} else {
			out[key] = s
		}
	case "posts_per_page", "posts_per_rss":
		if n, ok := v.(int); ok && n < 1 {
			out[key] = 1
		}
	case "image_output_format":
		if !isStr || (s != "" && s != "webp" && s != "webp-all") {
			return nil, cli.Errorf("'image_output_format' must be '', 'webp' or 'webp-all'")
		}
	case "permalink_structure":
		if !isStr {
			return nil, cli.Errorf("'%s' must be a string", key)
		}
		out[key] = normalizePermalinkStructure(s)
	case "category_base", "tag_base":
		if !isStr {
			return nil, cli.Errorf("'%s' must be a string", key)
		}
		out[key] = strings.Trim(s, "/ ")
	case "show_on_front":
		if s != "page" && s != "posts" {
			return nil, cli.Errorf("'show_on_front' must be 'posts' or 'page'")
		}
	case "default_ping_status", "default_comment_status":
		if s != "open" && s != "closed" {
			return nil, cli.Errorf("'%s' must be 'open' or 'closed'", key)
		}
	case "default_post_format":
		if !isStr || (s != "0" && store.CanonicalPostFormat(s) != s) {
			return nil, cli.Errorf("unknown post format %q", v)
		}
	case "admin_email":
		if !isStr || !validEmail(s) {
			return nil, cli.Errorf("'admin_email' must be an e-mail address")
		}
		out[key] = s
	case "WPLANG":
		if !isStr {
			return nil, cli.Errorf("'WPLANG' must be a locale")
		}
		if s == "en_US" {
			s = ""
		}
		if !validLanguage(s) {
			return nil, cli.Errorf("unknown locale %q", s)
		}
		out[key] = s
	}
	return out, nil
}

// normalizePermalinkStructure is the Permalinks screen's rule: "" (plain) stays, a structure gets
// a leading slash, and one without any %tag% is plain.
func normalizePermalinkStructure(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "plain" {
		return ""
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	if !strings.Contains(s, "%") {
		return ""
	}
	return s
}

// optionNeedsRestart reports options whose effect a running server applies only when it saves them
// itself (configureCache, language packs, plugin start/stop, trusted keys).
func optionNeedsRestart(key string) bool {
	return strings.HasPrefix(key, "wpgo_cache_") || strings.HasPrefix(key, "wpgo_opt_") || strings.HasPrefix(key, "native_") ||
		key == "WPLANG" || key == "plugin_trusted_keys"
}

// globToLike turns a shell glob (*, ?) into a LIKE pattern with '\' as the escape character.
func optGlobToLike(glob string) string {
	if glob == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// globMatch matches name against a glob with * and ? (no character classes).
func optGlobMatch(glob, name string) bool {
	if glob == "" {
		return true
	}
	g, n := []rune(glob), []rune(name)
	var match func(i, j int) bool
	match = func(i, j int) bool {
		for i < len(g) {
			switch g[i] {
			case '*':
				for k := j; k <= len(n); k++ {
					if match(i+1, k) {
						return true
					}
				}
				return false
			case '?':
				if j >= len(n) {
					return false
				}
			default:
				if j >= len(n) || g[i] != n[j] {
					return false
				}
			}
			i++
			j++
		}
		return j == len(n)
	}
	return match(0, 0)
}

// truncateCLI shortens s to max runes with an ellipsis.
func optTruncate(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func optJSONEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func optUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func optContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// --- JSON paths (option pluck / patch) ---------------------------------------------------

// jsonPathGet walks v along path: object keys by name, array items by index.
func jsonPathGet(v any, path []string) (any, error) {
	cur := v
	for i, p := range path {
		switch t := cur.(type) {
		case map[string]any:
			x, ok := t[p]
			if !ok {
				return nil, fmt.Errorf("no key '%s' at '%s'", p, strings.Join(path[:i], "."))
			}
			cur = x
		case []any:
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || n >= len(t) {
				return nil, fmt.Errorf("no index '%s' at '%s'", p, strings.Join(path[:i], "."))
			}
			cur = t[n]
		default:
			return nil, fmt.Errorf("'%s' is not an object or array", strings.Join(path[:i], "."))
		}
	}
	return cur, nil
}

// jsonPathPatch returns v with the value at path inserted (must not exist), updated (must exist)
// or deleted (must exist). Array items are addressed by index; insert at index len appends.
func jsonPathPatch(v any, action string, path []string, nv any) (any, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("missing key path")
	}
	where := func(i int) string {
		if i == 0 {
			return "the option"
		}
		return "'" + strings.Join(path[:i], ".") + "'"
	}
	var walk func(cur any, i int) (any, error)
	walk = func(cur any, i int) (any, error) {
		p := path[i]
		last := i == len(path)-1
		switch t := cur.(type) {
		case map[string]any:
			x, ok := t[p]
			if !last {
				if !ok {
					return nil, fmt.Errorf("no key '%s' in %s", p, where(i))
				}
				y, err := walk(x, i+1)
				if err != nil {
					return nil, err
				}
				t[p] = y
				return t, nil
			}
			switch action {
			case "insert":
				if ok {
					return nil, fmt.Errorf("key '%s' already exists in %s (use update)", p, where(i))
				}
				t[p] = nv
			case "update":
				if !ok {
					return nil, fmt.Errorf("no key '%s' in %s (use insert)", p, where(i))
				}
				t[p] = nv
			case "delete":
				if !ok {
					return nil, fmt.Errorf("no key '%s' in %s", p, where(i))
				}
				delete(t, p)
			}
			return t, nil
		case []any:
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("'%s' is not an array index in %s", p, where(i))
			}
			if !last {
				if n >= len(t) {
					return nil, fmt.Errorf("no index %d in %s", n, where(i))
				}
				y, err := walk(t[n], i+1)
				if err != nil {
					return nil, err
				}
				t[n] = y
				return t, nil
			}
			switch action {
			case "insert":
				if n > len(t) {
					return nil, fmt.Errorf("index %d is past the end of %s", n, where(i))
				}
				t = append(t[:n], append([]any{nv}, t[n:]...)...)
			case "update":
				if n >= len(t) {
					return nil, fmt.Errorf("no index %d in %s", n, where(i))
				}
				t[n] = nv
			case "delete":
				if n >= len(t) {
					return nil, fmt.Errorf("no index %d in %s", n, where(i))
				}
				t = append(t[:n], t[n+1:]...)
			}
			return t, nil
		}
		return nil, fmt.Errorf("%s is not an object or array", where(i))
	}
	return walk(v, 0)
}

// patchValue parses the value of `option patch`: JSON with asJSON; otherwise the type of the
// current value (bool, number) when there is one, else a string.
func patchValue(raw string, asJSON bool, cur any) (any, error) {
	if asJSON {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, cli.Errorf("invalid JSON value: %v", err)
		}
		return v, nil
	}
	switch cur.(type) {
	case bool:
		b, err := cli.ParseBool(raw)
		if err != nil {
			return nil, cli.Errorf("the current value is a boolean: use true or false (or --format=json)")
		}
		return b, nil
	case float64:
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, cli.Errorf("the current value is a number, got %q (or --format=json)", raw)
		}
		return f, nil
	}
	return raw, nil
}
