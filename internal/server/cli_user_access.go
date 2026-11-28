package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"wpgo/internal/cli"
	"wpgo/internal/store"
)

// Roles, capabilities, user meta, sessions and application passwords (see cli_user.go).

// cliUserRolesAfter computes a user's roles (primary first) after set / add / remove. role "" for
// remove means every role ("No role for this site"). changed is false when nothing would change.
func cliUserRolesAfter(op string, current []string, role string) (next []string, changed bool, err error) {
	switch op {
	case "set":
		next = []string{role}
		return next, !slices.Equal(current, next), nil
	case "add":
		if slices.Contains(current, role) {
			return slices.Clone(current), false, nil
		}
		return append(slices.Clone(current), role), true, nil
	case "remove":
		if role == "" {
			return []string{}, len(current) > 0, nil
		}
		if !slices.Contains(current, role) {
			return nil, false, fmt.Errorf("the user does not have the role '%s'", role)
		}
		next = []string{}
		for _, r := range current {
			if r != role {
				next = append(next, r)
			}
		}
		return next, true, nil
	}
	return nil, false, fmt.Errorf("unknown role operation %q", op)
}

// cliUserCapsAfter applies add (allow), deny or remove to the per-user capability exceptions. It
// returns the capabilities that changed.
func cliUserCapsAfter(op string, exc map[string]string, capsIn []string) (map[string]string, []string) {
	out := make(map[string]string, len(exc))
	for k, v := range exc {
		out[k] = v
	}
	var changed []string
	for _, cp := range capsIn {
		cp = strings.TrimSpace(cp)
		if cp == "" {
			continue
		}
		switch op {
		case "add", "deny":
			effect := "allow"
			if op == "deny" {
				effect = "deny"
			}
			if out[cp] != effect {
				out[cp] = effect
				changed = append(changed, cp)
			}
		case "remove":
			if _, ok := out[cp]; ok {
				delete(out, cp)
				changed = append(changed, cp)
			}
		}
	}
	return out, changed
}

// cliUserMetaValue turns a command-line value into the stored JSON: parsed with --format=json, else a string.
func cliUserMetaValue(raw, format string) (json.RawMessage, error) {
	if format == "json" {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("invalid JSON value: %v", err)
		}
		b, _ := json.Marshal(v)
		return b, nil
	}
	b, _ := json.Marshal(raw)
	return b, nil
}

// cliUserSecurityMetaKeys are meta keys that change how the user signs in.
var cliUserSecurityMetaKeys = map[string]string{
	"password_reset":          "a pending password-reset key",
	"adminsec_force_password": "a forced password change at the next login (\"admin\")",
	"_new_email":              "a pending e-mail address change",
}

// sortedKeys returns m's keys in order.
func cliUserSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// cliUserCapRows are the rows of list-caps for origin all, role or user.
func cliUserCapRows(origin string, u *store.User, roleCaps map[string]bool, exc map[string]string) []cli.Row {
	var rows []cli.Row
	switch origin {
	case "user":
		for _, k := range cliUserSortedKeys(exc) {
			rows = append(rows, cli.Row{"name": k, "source": "user", "effect": exc[k]})
		}
	case "role":
		for _, k := range cliUserSortedKeys(roleCaps) {
			if roleCaps[k] {
				rows = append(rows, cli.Row{"name": k, "source": "role", "effect": "allow"})
			}
		}
	default:
		for _, k := range cliUserSortedKeys(u.Caps) {
			if !u.Caps[k] {
				continue
			}
			src := "role"
			if exc[k] == "allow" && !roleCaps[k] {
				src = "user"
			}
			rows = append(rows, cli.Row{"name": k, "source": src, "effect": "allow"})
		}
	}
	return rows
}

// printCapNames prints rows as --format=list (one name per line) or through the formatters.
func cliUserPrintCaps(c *cli.Ctx, rows []cli.Row, fields []string) error {
	if c.Format == "list" && c.Field == "" && len(c.Fields) == 0 {
		for _, r := range rows {
			c.Println(r["name"])
		}
		return nil
	}
	if c.Format == "list" {
		c.Format = "table"
	}
	return c.Print(rows, cli.View{Fields: fields, ID: "name"})
}

// roleCapsUnion is the union of the capabilities of the given roles.
func (s *Server) cliUserRoleCapsUnion(ctx context.Context, roles []string) map[string]bool {
	out := map[string]bool{}
	for _, r := range roles {
		m, _ := s.roleCapabilityMap(ctx, r)
		for k, v := range m {
			if v {
				out[k] = true
			}
		}
	}
	return out
}

func registerUserAccessCLI(r *cli.Registry, e *cliEnv) {
	// --- roles ---------------------------------------------------------------------------
	roleCmd := func(op, path, short, long string, args []cli.Arg, examples []string) {
		r.Add(cli.Command{Path: path, Short: short, Long: long, Args: args, Examples: examples,
			Run: func(c *cli.Ctx) error { return cliUserRoleChange(c, e, op) }})
	}
	roleCmd("set", "user set-role", "Set a user's role (replaces every role).",
		"Replaces the user's roles with one role (default: the site's default_role). The last administrator\ncannot be demoted.",
		[]cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "role", Usage: "Role slug.", Optional: true}},
		[]string{"user set-role bob editor"})
	roleCmd("add", "user add-role", "Add a role to a user (kept next to the primary role).",
		"Adds an extra role; the primary role stays first. A user without a role gets it as primary role.",
		[]cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "role", Usage: "Role slug."}},
		[]string{"user add-role bob author"})
	roleCmd("remove", "user remove-role", "Remove a role from a user (or every role).",
		"Removes one role, or every role when none is given. The first remaining role becomes the primary\nrole. The last administrator cannot be demoted.",
		[]cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "role", Usage: "Role slug.", Optional: true}},
		[]string{"user remove-role bob author", "user remove-role bob"})

	// --- capabilities --------------------------------------------------------------------
	capCmd := func(op, path, short, long string, examples []string) {
		r.Add(cli.Command{Path: path, Short: short, Long: long,
			Args:     []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "cap", Usage: "Capability.", Repeat: true}},
			Examples: examples,
			Run:      func(c *cli.Ctx) error { return cliUserCapChange(c, e, op) }})
	}
	capCmd("add", "user add-cap", "Grant capabilities to a user (per-user allow exception).",
		"Adds per-user allow exceptions on top of the user's roles. A deny exception always wins.",
		[]string{"user add-cap bob upload_files", "user add-cap bob edit_theme_options moderate_comments"})
	capCmd("deny", "user deny-cap", "Deny capabilities to a user, whatever the roles grant.",
		"Adds per-user deny exceptions; they win over the roles and allow exceptions.",
		[]string{"user deny-cap bob publish_posts"})
	capCmd("remove", "user remove-cap", "Remove per-user capability exceptions (allow or deny).",
		"Removes per-user exceptions; the roles decide again.",
		[]string{"user remove-cap bob upload_files"})

	r.Add(cli.Command{
		Path:  "user list-caps",
		Short: "List a user's capabilities.",
		Long: "Lists the effective capabilities (roles plus allow exceptions minus deny exceptions). --origin=role\n" +
			"lists what the roles grant, --origin=user only the per-user exceptions (with their effect).",
		Args:          []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}},
		Flags:         []cli.Flag{{Name: "origin", Usage: "Which capabilities.", Default: "all", Values: []string{"all", "role", "user"}}},
		Formats:       []string{"list"},
		DefaultFormat: "list",
		Examples:      []string{"user list-caps admin", "user list-caps bob --origin=user --format=table"},
		Run: func(c *cli.Ctx) error {
			u, err := e.User(c.Context, c.Arg(0))
			if err != nil {
				return err
			}
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			exc, err := s.store.UserCapExceptions(c.Context, u.ID)
			if err != nil {
				return err
			}
			origin := c.Flag("origin")
			fields := []string{"name", "source"}
			if origin == "user" {
				fields = []string{"name", "effect"}
			}
			return cliUserPrintCaps(c, cliUserCapRows(origin, u, s.cliUserRoleCapsUnion(c.Context, u.Roles), exc), fields)
		},
	})

	// --- user meta -----------------------------------------------------------------------
	r.Noun("user meta", "Manage user meta (the users.meta JSON object).")
	r.Add(cli.Command{
		Path:     "user meta list",
		Short:    "List a user's meta.",
		Long:     "Lists users.meta. Profile fields (first_name, last_name, nickname, description) are not meta in Pursae:\nsee `user get` / `user update`.",
		Args:     []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}},
		Flags:    []cli.Flag{{Name: "keys", Usage: "Only these keys (comma-separated)."}},
		Examples: []string{"user meta list admin", "user meta list admin --keys=admin_color,locale --format=json"},
		Run: func(c *cli.Ctx) error {
			u, err := e.User(c.Context, c.Arg(0))
			if err != nil {
				return err
			}
			only := c.List("keys")
			var rows []cli.Row
			for _, k := range cliUserSortedKeys(u.Meta) {
				if len(only) > 0 && !slices.Contains(only, k) {
					continue
				}
				rows = append(rows, cli.Row{"user_id": u.ID, "meta_key": k, "meta_value": u.Meta[k]})
			}
			return c.Print(rows, cli.View{Fields: []string{"user_id", "meta_key", "meta_value"}, ID: "meta_key"})
		},
	})
	r.Add(cli.Command{
		Path:          "user meta get",
		Short:         "Get one meta value of a user.",
		Args:          []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "key", Usage: "Meta key."}},
		Formats:       []string{"plaintext"},
		DefaultFormat: "plaintext",
		Examples:      []string{"user meta get admin admin_color", "user meta get admin persisted_preferences --format=json"},
		Run: func(c *cli.Ctx) error {
			u, err := e.User(c.Context, c.Arg(0))
			if err != nil {
				return err
			}
			v, ok := u.Meta[c.Arg(1)]
			if !ok {
				return cli.Errorf("user %d has no meta key '%s'", u.ID, c.Arg(1))
			}
			return c.PrintValue(v)
		},
	})
	metaWrite := func(verb, short string) {
		r.Add(cli.Command{
			Path:  "user meta " + verb,
			Short: short,
			Long: "Stores a string, or any JSON value with --format=json (\"-\" reads the value from stdin). Pursae keeps\n" +
				"one value per key. Raw meta writes bypass validation: keys such as password_reset and\n" +
				"adminsec_force_password change how the user signs in.",
			Args:          []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "key", Usage: "Meta key."}, {Name: "value", Usage: "Value, or - for stdin."}},
			Formats:       []string{"plaintext"},
			DefaultFormat: "plaintext",
			Examples:      []string{"user meta " + verb + " bob admin_color midnight", "user meta " + verb + " bob show_admin_bar_front false --format=json"},
			Run:           func(c *cli.Ctx) error { return cliUserMetaSet(c, e, verb == "add") },
		})
	}
	metaWrite("update", "Set a user meta value.")
	metaWrite("add", "Add a user meta value (fails when the key exists).")
	r.Add(cli.Command{
		Path:     "user meta delete",
		Short:    "Delete user meta keys.",
		Args:     []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "key", Usage: "Meta key.", Repeat: true}},
		Examples: []string{"user meta delete bob admin_color"},
		Run: func(c *cli.Ctx) error {
			ctx := c.Context
			u, err := e.User(ctx, c.Arg(0))
			if err != nil {
				return err
			}
			keys := c.Args[1:]
			var found []string
			for _, k := range keys {
				if _, ok := u.Meta[k]; ok {
					found = append(found, k)
				}
			}
			if len(found) == 0 {
				return cli.Errorf("user %d has none of the meta keys: %s", u.ID, strings.Join(keys, ", "))
			}
			for _, k := range found {
				if why, ok := cliUserSecurityMetaKeys[k]; ok {
					c.Warning("'%s' is security-relevant meta (%s).", k, why)
				}
			}
			pool, err := e.Pool(ctx)
			if err != nil {
				return err
			}
			if _, err := pool.Exec(ctx, `UPDATE users SET meta = meta - $2::text[] WHERE id = $1`, u.ID, found); err != nil {
				return err
			}
			c.Success("Deleted meta %s of user %d.", strings.Join(found, ", "), u.ID)
			return e.PurgeAll(ctx)
		},
	})

	// --- sessions ------------------------------------------------------------------------
	r.Noun("user session", "List and end a user's login sessions.")
	r.Add(cli.Command{
		Path:     "user session list",
		Short:    "List a user's active sessions.",
		Args:     []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}},
		Examples: []string{"user session list admin", "user session list admin --format=json"},
		Run: func(c *cli.Ctx) error {
			u, err := e.User(c.Context, c.Arg(0))
			if err != nil {
				return err
			}
			pool, err := e.Pool(c.Context)
			if err != nil {
				return err
			}
			rs, err := pool.Query(c.Context, `SELECT id, ip, user_agent, created_at, last_seen_at, expires_at FROM sessions
				WHERE user_id = $1 AND expires_at > now() ORDER BY last_seen_at DESC, id DESC`, u.ID)
			if err != nil {
				return err
			}
			defer rs.Close()
			var rows []cli.Row
			for rs.Next() {
				var id int64
				var ip, ua string
				var created, seen, expires time.Time
				if err := rs.Scan(&id, &ip, &ua, &created, &seen, &expires); err != nil {
					return err
				}
				rows = append(rows, cli.Row{"id": id, "ip": ip, "user_agent": ua, "created_at": created, "last_seen_at": seen, "expires_at": expires})
			}
			if err := rs.Err(); err != nil {
				return err
			}
			return c.Print(rows, cli.View{Fields: []string{"id", "ip", "user_agent", "created_at", "last_seen_at", "expires_at"}, ID: "id"})
		},
	})
	r.Add(cli.Command{
		Path:     "user session destroy",
		Short:    "End one session of a user, or all of them.",
		Long:     "Logs the user out: the session rows are deleted, so a running server refuses them at the next request.",
		Args:     []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "session-id", Usage: "Session ID (from `user session list`).", Optional: true}},
		Flags:    []cli.Flag{{Name: "all", Bool: true, Usage: "End every session of the user."}},
		Examples: []string{"user session destroy bob 42", "user session destroy bob --all --yes"},
		Run: func(c *cli.Ctx) error {
			ctx := c.Context
			all := c.Bool("all")
			if all == (c.Arg(1) != "") {
				return cli.Usagef("give either a session ID or --all")
			}
			u, err := e.User(ctx, c.Arg(0))
			if err != nil {
				return err
			}
			s, err := e.Server(ctx)
			if err != nil {
				return err
			}
			if all {
				if err := c.Confirm("End every session of %s?", u.Login); err != nil {
					return err
				}
				tag, err := s.store.Pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, u.ID)
				if err != nil {
					return err
				}
				cliUserAudit(c, e, s, asEvent{Event: "sessions_logged_out", Severity: "notice", Object: u.Login, Message: "Every session of " + u.Login + " ended from the command line."})
				c.Success("Destroyed %d session(s) of %s.", tag.RowsAffected(), u.Login)
				return nil
			}
			id, err := cli.ParseID(c.Arg(1))
			if err != nil {
				return err
			}
			tag, err := s.store.Pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1 AND user_id = $2`, id, u.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return cli.Errorf("user %s has no session %d", u.Login, id)
			}
			cliUserAudit(c, e, s, asEvent{Event: "sessions_logged_out", Object: u.Login, Message: "One session of " + u.Login + " ended."})
			c.Success("Destroyed session %d of %s.", id, u.Login)
			return nil
		},
	})

	// --- application passwords -----------------------------------------------------------
	r.Noun("user application-password", "Manage a user's application passwords (REST API and MCP credentials).")
	appFields := []string{"uuid", "app_id", "name", "scopes", "created", "last_used", "last_ip"}
	appRow := func(ap *appPassword) cli.Row {
		row := cli.Row{"uuid": ap.UUID, "app_id": ap.AppID, "name": ap.Name, "scopes": appPasswordScopeLabel(ap.Scopes),
			"created": ap.CreatedAt.UTC(), "last_used": "", "last_ip": ap.LastIP}
		if ap.LastUsed != nil {
			row["last_used"] = ap.LastUsed.UTC()
		}
		return row
	}
	r.Add(cli.Command{
		Path:     "user application-password list",
		Short:    "List a user's application passwords.",
		Args:     []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}},
		Examples: []string{"user application-password list admin", "user application-password list admin --format=ids"},
		Run: func(c *cli.Ctx) error {
			u, err := e.User(c.Context, c.Arg(0))
			if err != nil {
				return err
			}
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			list, err := s.appPasswords(c.Context, u.ID)
			if err != nil {
				return err
			}
			rows := make([]cli.Row, 0, len(list))
			for _, ap := range list {
				rows = append(rows, appRow(ap))
			}
			return c.Print(rows, cli.View{Fields: []string{"uuid", "name", "scopes", "created", "last_used", "last_ip"}, ID: "uuid"})
		},
	})
	r.Add(cli.Command{
		Path:  "user application-password get",
		Short: "Get one application password's details (never the password).",
		Args:  []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "uuid", Usage: "Application password UUID."}},
		Run: func(c *cli.Ctx) error {
			u, err := e.User(c.Context, c.Arg(0))
			if err != nil {
				return err
			}
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			ap, err := s.appPasswordByUUID(c.Context, u.ID, c.Arg(1))
			if err != nil {
				return cli.Errorf("user %s has no application password %s", u.Login, c.Arg(1))
			}
			return c.PrintItem(appRow(ap), appFields)
		},
	})
	r.Add(cli.Command{
		Path:  "user application-password create",
		Short: "Create an application password (printed once).",
		Long: "Creates an application password for the REST API and MCP. Without --scopes it may use content and\n" +
			"media only; --scopes=full_access keeps every capability of the user. Scopes: full_access, " + cliUserScopeSlugs() + ".",
		Args: []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "app-name", Usage: "Unique name of the application."}},
		Flags: []cli.Flag{
			{Name: "app-id", Usage: "Application UUID."},
			{Name: "scopes", Usage: "Comma-separated scopes (default: content,media)."},
			{Name: "porcelain", Bool: true, Usage: "Print only the password."},
		},
		Examples: []string{"user application-password create admin 'Deploy bot' --scopes=content,media", "user application-password create admin CI --scopes=full_access --porcelain"},
		Run: func(c *cli.Ctx) error {
			u, err := e.User(c.Context, c.Arg(0))
			if err != nil {
				return err
			}
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			scopes := c.List("scopes")
			for _, sc := range scopes {
				if !strings.EqualFold(sc, appScopeFullAccess) && appScopeBySlug(strings.ToLower(sc)) == nil {
					return cli.Errorf("unknown scope '%s' (known: full_access, %s)", sc, cliUserScopeSlugs())
				}
			}
			plain, ap, err := s.createAppPassword(c.Context, u.ID, c.Arg(1), c.Flag("app-id"), scopes...)
			if errors.Is(err, errAppPasswordDuplicateName) || errors.Is(err, errAppPasswordEmptyName) {
				return cli.Errorf("%s", err.Error())
			}
			if err != nil {
				return err
			}
			if c.Bool("porcelain") {
				c.Println(chunkAppPassword(plain))
				return nil
			}
			c.Success("Created application password %s (%s) for %s.", ap.UUID, appPasswordScopeLabel(ap.Scopes), u.Login)
			c.Println("Password: " + chunkAppPassword(plain))
			return nil
		},
	})
	r.Add(cli.Command{
		Path:     "user application-password delete",
		Short:    "Revoke application passwords.",
		Args:     []cli.Arg{{Name: "user", Usage: "User ID, e-mail or login."}, {Name: "uuid", Usage: "Application password UUID.", Optional: true, Repeat: true}},
		Flags:    []cli.Flag{{Name: "all", Bool: true, Usage: "Revoke every application password of the user."}},
		Examples: []string{"user application-password delete admin 6633824d-c1d7-4f79-9dd5-4586f734d69e --yes", "user application-password delete admin --all --yes"},
		Run: func(c *cli.Ctx) error {
			ctx := c.Context
			all := c.Bool("all")
			if all == (len(c.Args) > 1) {
				return cli.Usagef("give either application password UUIDs or --all")
			}
			u, err := e.User(ctx, c.Arg(0))
			if err != nil {
				return err
			}
			s, err := e.Server(ctx)
			if err != nil {
				return err
			}
			if all {
				if err := c.Confirm("Revoke every application password of %s?", u.Login); err != nil {
					return err
				}
				n, err := s.deleteAllAppPasswords(ctx, u.ID)
				if err != nil {
					return err
				}
				c.Success("Revoked %d application password(s) of %s.", n, u.Login)
				return nil
			}
			var list []*appPassword
			for _, id := range c.Args[1:] {
				ap, err := s.appPasswordByUUID(ctx, u.ID, id)
				if err != nil {
					return cli.Errorf("user %s has no application password %s", u.Login, id)
				}
				list = append(list, ap)
			}
			if err := c.Confirm("Revoke %d application password(s) of %s?", len(list), u.Login); err != nil {
				return err
			}
			for _, ap := range list {
				if err := s.deleteAppPassword(ctx, ap.ID); err != nil {
					return err
				}
				c.Success("Revoked application password %s (%s).", ap.UUID, ap.Name)
			}
			return nil
		},
	})

	// --- role list / cap list ------------------------------------------------------------
	r.Add(cli.Command{
		Path:     "role list",
		Short:    "List roles.",
		Long:     "Fields: name, role, users (count), is_system, capabilities_count, capabilities.",
		Examples: []string{"role list", "role list --fields=role,capabilities_count --format=csv", "role list --format=ids"},
		Run: func(c *cli.Ctx) error {
			rows, err := cliUserRoleRows(c.Context, e)
			if err != nil {
				return err
			}
			return c.Print(rows, cli.View{Fields: []string{"name", "role", "users"}, ID: "role"})
		},
	})
	r.Add(cli.Command{
		Path:  "role exists",
		Short: "Check whether a role exists (exit code 0 or 1).",
		Args:  []cli.Arg{{Name: "role", Usage: "Role slug."}},
		Run: func(c *cli.Ctx) error {
			st, err := e.Store(c.Context)
			if err != nil {
				return err
			}
			roles, err := st.Roles(c.Context)
			if err != nil {
				return err
			}
			if !roleListed(roles, c.Arg(0)) {
				return cli.Exit(cli.ExitFailure)
			}
			c.Success("Role with ID '%s' exists.", c.Arg(0))
			return nil
		},
	})
	r.Add(cli.Command{
		Path:          "cap list",
		Short:         "List the capabilities of a role.",
		Args:          []cli.Arg{{Name: "role", Usage: "Role slug."}},
		Formats:       []string{"list"},
		DefaultFormat: "list",
		Examples:      []string{"cap list editor", "cap list editor --format=count"},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			m, ok := s.roleCapabilityMap(c.Context, c.Arg(0))
			if !ok {
				return cli.Errorf("'%s' role not found", c.Arg(0))
			}
			var rows []cli.Row
			for _, k := range cliUserSortedKeys(m) {
				if m[k] {
					rows = append(rows, cli.Row{"name": k})
				}
			}
			return cliUserPrintCaps(c, rows, []string{"name"})
		},
	})
}

// cliUserScopeSlugs lists the application password scopes.
func cliUserScopeSlugs() string {
	out := make([]string, 0, len(appScopes))
	for _, sc := range appScopes {
		out = append(out, sc.Slug)
	}
	return strings.Join(out, ", ")
}

// cliUserRoleRows are the rows of `role list`.
func cliUserRoleRows(ctx context.Context, e *cliEnv) ([]cli.Row, error) {
	st, err := e.Store(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := st.RoleCounts(ctx)
	if err != nil {
		return nil, err
	}
	rs, err := st.Pool.Query(ctx, `SELECT slug, name, is_system, capabilities FROM roles ORDER BY
		array_position(ARRAY['administrator','editor','author','contributor','subscriber'], slug), name`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var rows []cli.Row
	for rs.Next() {
		var slug, name string
		var system bool
		var raw []byte
		if err := rs.Scan(&slug, &name, &system, &raw); err != nil {
			return nil, err
		}
		m := map[string]bool{}
		_ = json.Unmarshal(raw, &m)
		var granted []string
		for _, k := range cliUserSortedKeys(m) {
			if m[k] {
				granted = append(granted, k)
			}
		}
		rows = append(rows, cli.Row{"name": name, "role": slug, "users": counts[slug], "is_system": system,
			"capabilities_count": len(granted), "capabilities": granted})
	}
	return rows, rs.Err()
}

func cliUserRoleChange(c *cli.Ctx, e *cliEnv, op string) error {
	ctx := c.Context
	u, err := e.User(ctx, c.Arg(0))
	if err != nil {
		return err
	}
	s, err := e.Server(ctx)
	if err != nil {
		return err
	}
	role := c.Arg(1)
	if op == "set" && role == "" {
		role = s.settings.String("default_role")
	}
	if role != "" {
		roles, err := s.store.Roles(ctx)
		if err != nil {
			return err
		}
		if !roleListed(roles, role) {
			return cli.Errorf("invalid role: '%s' (see '%s role list')", role, c.Prog)
		}
	}
	next, changed, err := cliUserRolesAfter(op, u.Roles, role)
	if err != nil {
		return cli.Errorf("%s: %v", u.Login, err)
	}
	if !changed {
		c.Warning("%s already has that role set; nothing changed.", u.Login)
		return nil
	}
	if s.removingLastAdmin(ctx, u, next) {
		return cli.Errorf("%s is the last administrator; give another user the administrator role first", u.Login)
	}
	primary, extra := "", []string(nil)
	if len(next) > 0 {
		primary, extra = next[0], next[1:]
	}
	if err := s.store.SetUserRoles(ctx, u.ID, primary, extra); err != nil {
		return err
	}
	_ = s.auth.DestroyOthers(ctx, u.ID, 0) // sessions opened under the old roles end (endSessionsAfterAccessChange)
	before, after := strings.Join(u.Roles, ","), strings.Join(next, ",")
	if s.adminsecOn() && before != after {
		if before == "" {
			before = "none"
		}
		shown := after
		if shown == "" {
			shown = "none"
		}
		cliUserAudit(c, e, s, asEvent{Event: "role_changed", Severity: "notice", Object: u.Login, Message: "Role of " + u.Login + " changed from " + before + " to " + shown + "."})
	}
	if after == "" {
		c.Success("Removed every role of %s.", u.Login)
	} else {
		c.Success("Roles of %s: %s.", u.Login, after)
	}
	return e.PurgeAll(ctx)
}

func cliUserCapChange(c *cli.Ctx, e *cliEnv, op string) error {
	ctx := c.Context
	u, err := e.User(ctx, c.Arg(0))
	if err != nil {
		return err
	}
	st, err := e.Store(ctx)
	if err != nil {
		return err
	}
	exc, err := st.UserCapExceptions(ctx, u.ID)
	if err != nil {
		return err
	}
	next, changed := cliUserCapsAfter(op, exc, c.Args[1:])
	if len(changed) == 0 {
		if op == "remove" {
			return cli.Errorf("%s has no per-user exception for: %s", u.Login, strings.Join(c.Args[1:], ", "))
		}
		c.Warning("%s already has these exceptions; nothing changed.", u.Login)
		return nil
	}
	if err := st.SetUserCapExceptions(ctx, u.ID, next); err != nil {
		return err
	}
	_, _ = st.Pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, u.ID) // sessions opened under the old capabilities end
	switch op {
	case "add":
		c.Success("Added capabilities to %s: %s.", u.Login, strings.Join(changed, ", "))
	case "deny":
		c.Success("Denied capabilities to %s: %s.", u.Login, strings.Join(changed, ", "))
	default:
		c.Success("Removed capability exceptions of %s: %s.", u.Login, strings.Join(changed, ", "))
	}
	return e.PurgeAll(ctx)
}

func cliUserMetaSet(c *cli.Ctx, e *cliEnv, add bool) error {
	ctx := c.Context
	u, err := e.User(ctx, c.Arg(0))
	if err != nil {
		return err
	}
	key := c.Arg(1)
	if strings.TrimSpace(key) == "" {
		return cli.Usagef("the meta key cannot be empty")
	}
	if _, exists := u.Meta[key]; add && exists {
		return cli.Errorf("user %d already has the meta key '%s'; use 'user meta update'", u.ID, key)
	}
	raw, err := cliUserReadValue(c, c.Arg(2))
	if err != nil {
		return err
	}
	val, err := cliUserMetaValue(raw, c.Format)
	if err != nil {
		return cli.Errorf("%v", err)
	}
	if why, ok := cliUserSecurityMetaKeys[key]; ok {
		c.Warning("'%s' is security-relevant meta (%s).", key, why)
	}
	pool, err := e.Pool(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET meta = meta || jsonb_build_object($2::text, $3::jsonb) WHERE id = $1`, u.ID, key, string(val)); err != nil {
		return err
	}
	if add {
		c.Success("Added meta '%s' of user %d.", key, u.ID)
	} else {
		c.Success("Updated meta '%s' of user %d.", key, u.ID)
	}
	return e.PurgeAll(ctx)
}
