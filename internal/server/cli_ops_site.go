package server

import (
	"fmt"
	"html"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"wpgo/internal/cli"
	"wpgo/internal/i18n"
	"wpgo/internal/mail"
)

// The backup, language, mail and site commands of the ops group (registerOpsCLI in cli_ops.go).

// --- backup -------------------------------------------------------------------------

// backupListFields are the default columns of `backup list`.
var backupListFields = []string{"name", "size", "kind", "created", "parts", "note"}

func registerOpsBackupCLI(r *cli.Registry, e *cliEnv) {
	r.Noun("backup", "Create, list, restore and delete site backups (Pursae Backup Manager archives).")
	registerBackupRemoteCLI(r, e)

	r.Add(cli.Command{
		Path:  "backup list",
		Short: "List the backup archives in the backups directory.",
		Long:  "Lists PURSAE_BACKUPS_DIR (default <data>/backups), newest first. Unfinished .zip.part files older\nthan an hour are swept, as on the Backups tab.",
		Examples: []string{
			"backup list",
			"backup list --fields=name,size,site_url,schema --format=json",
		},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			var out []cli.Row
			for _, be := range s.bmgrList() {
				row := cli.Row{"name": be.Name, "size": bmgrHuman(be.Size), "bytes": be.Size, "created": be.Modified,
					"kind": "", "note": "", "site_url": "", "schema": "", "version": "", "parts": []string{}, "error": be.Err}
				if m := be.M; m != nil {
					row["kind"], row["note"], row["site_url"], row["schema"], row["version"] = defaultStr(m.Kind, "manual"), m.Note, m.SiteURL, m.Schema, m.Version
					row["parts"] = bmgrManifestCats(m)
					if !m.Created.IsZero() {
						row["created"] = m.Created
					}
				}
				out = append(out, row)
			}
			return c.Print(out, cli.View{Fields: backupListFields, ID: "name"})
		},
	})

	r.Add(cli.Command{
		Path:  "backup create",
		Short: "Write a backup archive of the site.",
		Long: "Writes a .zip with the chosen parts (database rows as JSONL plus their files) into the backups\n" +
			"directory, the same archive as Backup Manager › Create. It reads the uploads, themes and plugins\n" +
			"directories, so run it with the server's environment (in Docker: docker exec).\n\n" +
			"Parts: " + strings.Join(bmgrCategoryKeys(), ", ") + " (default: all).",
		Flags: []cli.Flag{
			{Name: "parts", Usage: "Comma-separated parts, or all.", Default: "all"},
			{Name: "users", Usage: "Which users go with the users part: all, roles:<role,…> or ids:<id,…>.", Default: "all"},
			{Name: "note", Usage: "A note stored in the archive (max 300 characters)."},
			{Name: "porcelain", Bool: true, Usage: "Print only the archive name."},
		},
		Examples: []string{
			"backup create",
			"backup create --parts=content,media,settings --note=\"before the redesign\"",
			"backup create --parts=users --users=roles:administrator,editor --porcelain",
		},
		Run: func(c *cli.Ctx) error {
			cats, err := bmgrParseParts(c.Flag("parts"))
			if err != nil {
				return err
			}
			mode, roles, ids, err := bmgrParseUsers(c.Flag("users"))
			if err != nil {
				return err
			}
			note := strings.TrimSpace(c.Flag("note"))
			if len(note) > 300 {
				note = note[:300]
			}
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			if !s.bmgrOn() {
				c.Warning("Pursae Backup Manager is inactive; the archive is written anyway.")
			}
			j, err := s.bmgrStart("backup")
			if err != nil {
				return err
			}
			stop := opsFollowBackupLog(c, s, j, c.Bool("porcelain"))
			name, err := s.bmgrRunBackup(c.Context, j, bmgrSelection{Categories: cats, UserMode: mode, Roles: roles, UserIDs: ids, Note: note})
			s.bmgrUpdate(j, func(j *bmgrJob) { j.File = name })
			s.bmgrFinish(j, err)
			stop()
			if err != nil {
				return err
			}
			if c.Bool("porcelain") {
				c.Println(name)
				return nil
			}
			if j.Warnings > 0 {
				c.Warning("%d file(s) could not be read and are not in the archive (see the log above).", j.Warnings)
			}
			c.Success("Backup written: %s", filepath.Join(s.bmgrDir(), name))
			return nil
		},
	})

	r.Add(cli.Command{
		Path:  "backup restore",
		Short: "Restore parts of a backup archive over this site.",
		Long: "Replaces the chosen parts of this site with the archive's (one database transaction; files are\n" +
			"staged first). By default every part in the archive is restored, a safety snapshot of the site is\n" +
			"taken first, and the archive's site address is rewritten to this site's. <file> is an archive\n" +
			"name from `backup list`, or a path to a .zip, which is first copied into the backups directory.\n\n" +
			"Running servers get the restored settings live and drop their page cache; restored plugins and\n" +
			"themes need a server restart.",
		Args: []cli.Arg{{Name: "file", Usage: "Archive name or path."}},
		Flags: []cli.Flag{
			{Name: "parts", Usage: "Comma-separated parts to restore (default: everything in the archive)."},
			{Name: "rewrite-url", Bool: true, Default: "true", Usage: "Rewrite the archive's site address to this site's (--no-rewrite-url keeps it)."},
			{Name: "safety", Bool: true, Default: "true", Usage: "Take a safety snapshot first (--no-safety skips it)."},
		},
		Examples: []string{
			"backup restore manual-20260101-120000-abcd1234.zip --yes",
			"backup restore ./site.zip --parts=content,media --no-safety --yes",
		},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			name, err := opsBackupName(s, c.Arg(0))
			if err != nil {
				return err
			}
			m, err := bmgrReadManifest(s.bmgrPath(name))
			if err != nil {
				return err
			}
			cats := bmgrManifestCats(m)
			if c.Has("parts") {
				want, err := bmgrParseParts(c.Flag("parts"))
				if err != nil {
					return err
				}
				var keep []string
				for _, k := range want {
					if slices.Contains(cats, k) {
						keep = append(keep, k)
					} else {
						c.Warning("the archive has no '%s' part.", k)
					}
				}
				cats = keep
			}
			if len(cats) == 0 {
				return cli.Errorf("nothing to restore: select at least one part")
			}
			if _, _, err := s.bmgrMakePlan(c.Context, m, cats); err != nil {
				return err
			}
			actor, err := e.Actor(c)
			if err != nil {
				return err
			}
			if err := c.Confirm("Restore %s from %s over this site? The current data of those parts is replaced.", strings.Join(cats, ", "), name); err != nil {
				return err
			}
			j, err := s.bmgrStart("restore")
			if err != nil {
				return err
			}
			stop := opsFollowBackupLog(c, s, j, false)
			err = s.bmgrRunRestore(c.Context, j, bmgrRestoreReq{File: name, Categories: cats, RewriteURL: c.Bool("rewrite-url"), Safety: c.Bool("safety"), Me: actor.ID})
			s.bmgrFinish(j, err)
			stop()
			if err != nil {
				return err
			}
			// The engine purges after the database part; files-only restores (media) need it too.
			if perr := e.PurgeAll(c.Context); perr != nil {
				c.Warning("page cache purge: %v", perr)
			}
			if slices.Contains(cats, "plugins") || slices.Contains(cats, "themes") {
				c.Warning("plugins or themes were restored: restart the server to load them.")
			}
			c.Success("Restored %s from %s.", strings.Join(cats, ", "), name)
			return nil
		},
	})

	r.Add(cli.Command{
		Path:     "backup delete",
		Short:    "Delete backup archives.",
		Args:     []cli.Arg{{Name: "file", Usage: "Archive name (from backup list).", Repeat: true}},
		Examples: []string{"backup delete manual-20260101-120000-abcd1234.zip --yes"},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			var paths []string
			for _, a := range c.Args {
				p := s.bmgrPath(a)
				if p == "" {
					return cli.Errorf("backup '%s' not found in %s", a, s.bmgrDir())
				}
				paths = append(paths, p)
			}
			if err := c.Confirm("Delete %d backup archive(s) permanently?", len(paths)); err != nil {
				return err
			}
			for _, p := range paths {
				if err := os.Remove(p); err != nil {
					return err
				}
				c.Log("Deleted %s", filepath.Base(p))
			}
			c.Success("Deleted %d backup(s).", len(paths))
			return nil
		},
	})
}

// bmgrParseParts turns --parts into category keys ("all" or empty: every category).
func bmgrParseParts(v string) ([]string, error) {
	list := cli.SplitList(v)
	if len(list) == 0 || slices.Contains(list, "all") {
		return bmgrCategoryKeys(), nil
	}
	var out []string
	for _, k := range list {
		if bmgrCategoryByKey(k) == nil {
			return nil, cli.Usagef("unknown part '%s' (one of %s, or all)", k, strings.Join(bmgrCategoryKeys(), ", "))
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out, nil
}

// bmgrParseUsers turns --users (all | roles:<r,…> | ids:<n,…>) into a bmgrSelection user mode.
func bmgrParseUsers(v string) (mode string, roles []string, ids []int64, err error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "" || v == "all":
		return "all", nil, nil, nil
	case strings.HasPrefix(v, "roles:"):
		roles = cli.SplitList(strings.TrimPrefix(v, "roles:"))
		if len(roles) == 0 {
			return "", nil, nil, cli.Usagef("--users=roles: needs at least one role")
		}
		return "roles", roles, nil, nil
	case strings.HasPrefix(v, "ids:"):
		for _, s := range cli.SplitList(strings.TrimPrefix(v, "ids:")) {
			id, err := cli.ParseID(s)
			if err != nil {
				return "", nil, nil, err
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return "", nil, nil, cli.Usagef("--users=ids: needs at least one user ID")
		}
		return "ids", nil, ids, nil
	}
	return "", nil, nil, cli.Usagef("--users must be all, roles:<role,…> or ids:<id,…>")
}

// opsBackupName resolves a restore argument: an archive name in the backups directory, or a path
// to a .zip that is copied in first (as Backup Manager › Import does with an upload).
func opsBackupName(s *Server, arg string) (string, error) {
	if s.bmgrPath(arg) != "" {
		return arg, nil
	}
	fi, err := os.Stat(arg)
	if err != nil || !fi.Mode().IsRegular() {
		return "", cli.Errorf("backup '%s' not found (neither in %s nor as a file)", arg, s.bmgrDir())
	}
	if _, err := bmgrReadManifest(arg); err != nil {
		return "", cli.Errorf("%s is not a usable backup: %v", arg, err)
	}
	if err := os.MkdirAll(s.bmgrDir(), 0o700); err != nil {
		return "", err
	}
	name := "uploaded-" + time.Now().Format("20060102-150405") + "-" + bmgrToken() + ".zip"
	dst := filepath.Join(s.bmgrDir(), name)
	in, err := os.Open(arg)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(dst+".part", dst)
	}
	if err != nil {
		os.Remove(dst + ".part")
		return "", err
	}
	return name, nil
}

// opsFollowBackupLog prints a backup or restore job's log lines while it runs (unless quiet).
// The returned stop prints what is left and waits for the printer.
func opsFollowBackupLog(c *cli.Ctx, s *Server, j *bmgrJob, quiet bool) (stop func()) {
	if quiet || c.Quiet {
		return func() {}
	}
	var printed []string // the lines printed so far (the job keeps the last 300)
	flush := func() {
		s.bmgr.mu.Lock()
		lines := append([]string(nil), j.Log...)
		s.bmgr.mu.Unlock()
		for _, l := range opsNewLogLines(printed, lines) {
			c.Log("%s", l)
			printed = append(printed, l)
		}
		if len(printed) > 300 {
			printed = printed[len(printed)-300:]
		}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				flush()
				return
			case <-t.C:
				flush()
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// opsNewLogLines returns the lines of cur that come after what was printed: cur is a window of
// the job's log (it drops old lines), so the overlap is found by the last printed line.
func opsNewLogLines(printed, cur []string) []string {
	if len(printed) == 0 {
		return cur
	}
	last := printed[len(printed)-1]
	for i := len(cur) - 1; i >= 0; i-- {
		if cur[i] == last {
			return cur[i+1:]
		}
	}
	return cur
}

// --- language -----------------------------------------------------------------------

// langDir is where language packs live: PURSAE_LANG_DIR, else <data>/languages (initLanguages).
func (e *cliEnv) langDir() string {
	if d := e.cfg.LangDir; d != "" {
		return d
	}
	return filepath.Join(opsDirOf(e.cfg.UploadsDir), "languages")
}

// langFileRe matches the files of one locale in the languages directory: the core packs
// (admin-, admin-network-, continents-cities-, plain), their script JSON (<locale>-<md5>.json)
// and, in plugins/, <slug>-<locale>.mo|.po|.l10n.php and <slug>-<locale>-<md5>.json.
func langFileRe(locale string) *regexp.Regexp {
	l := regexp.QuoteMeta(locale)
	return regexp.MustCompile(`^(?:(?:admin-network-|admin-|continents-cities-)?` + l + `(?:\.mo|\.po|\.l10n\.php)|` + l + `-[0-9a-f]{32}\.json|` +
		`plugins/[a-z0-9._-]+-` + l + `(?:\.mo|\.po|\.l10n\.php|-[0-9a-f]{32}\.json))$`)
}

// langFiles lists the files of locale under dir (paths relative to dir, with "/").
func langFiles(dir, locale string) []string {
	re := langFileRe(locale)
	var out []string
	for _, sub := range []string{"", "plugins"} {
		ents, _ := os.ReadDir(filepath.Join(dir, sub))
		for _, en := range ents {
			if en.IsDir() {
				continue
			}
			rel := en.Name()
			if sub != "" {
				rel = sub + "/" + rel
			}
			if re.MatchString(rel) {
				out = append(out, rel)
			}
		}
	}
	return out
}

// langStatus is a locale's status in `language core list`.
func langStatus(locale, active string, installed map[string]bool) string {
	switch {
	case locale == active:
		return "active"
	case locale == "en_US" || installed[locale]:
		return "installed"
	}
	return "uninstalled"
}

func registerOpsLanguageCLI(r *cli.Registry, e *cliEnv) {
	r.Noun("language", "Install and activate language packs.")
	r.Noun("language core", "Manage the site language (core and built-in plugin translations).")

	r.Add(cli.Command{
		Path:  "language core list",
		Short: "List the available languages and which are installed and active.",
		Flags: []cli.Flag{
			{Name: "status", Usage: "Only languages with this status.", Values: []string{"active", "installed", "uninstalled"}},
		},
		Examples: []string{"language core list --status=installed", "language core list --field=language"},
		Run: func(c *cli.Ctx) error {
			set, err := e.Settings(c.Context)
			if err != nil {
				return err
			}
			active := defaultStr(set.String("WPLANG"), "en_US")
			installed := map[string]bool{}
			for _, l := range i18n.NewManager(e.langDir(), nil).Installed() {
				installed[l] = true
			}
			langs := append([]i18n.Language{{Locale: "en_US", English: "English (United States)", Native: "English (United States)"}}, i18n.Languages()...)
			var out []cli.Row
			for _, l := range langs {
				st := langStatus(l.Locale, active, installed)
				if f := c.Flag("status"); f != "" && f != st {
					continue
				}
				out = append(out, cli.Row{"language": l.Locale, "english_name": l.English, "native_name": l.Native, "status": st})
			}
			return c.Print(out, cli.View{Fields: []string{"language", "english_name", "native_name", "status"}, ID: "language"})
		},
	})

	r.Add(cli.Command{
		Path:  "language core install",
		Short: "Download language packs.",
		Long: "Downloads the WordPress core pack of each locale from the WordPress.org translation API, then the\n" +
			"translations of the plugins the built-in plugins share strings with, into PURSAE_LANG_DIR (default\n" +
			"<data>/languages). --activate then makes the (last) locale the site language.",
		Args: []cli.Arg{{Name: "language", Usage: "Locale, e.g. de_DE.", Repeat: true}},
		Flags: []cli.Flag{
			{Name: "activate", Bool: true, Usage: "Make the language the site language after installing it."},
		},
		Examples: []string{"language core install de_DE", "language core install tr_TR --activate"},
		Run: func(c *cli.Ctx) error {
			for _, l := range c.Args {
				if l == "" || !validLanguage(l) {
					return cli.Errorf("invalid language '%s' (see %s language core list)", l, c.Prog)
				}
			}
			mgr := i18n.NewManager(e.langDir(), nil)
			in := &i18n.Installer{API: e.cfg.TranslationsAPI}
			for _, l := range c.Args {
				if l == "en_US" {
					c.Log("en_US is built in.")
					continue
				}
				if mgr.IsInstalled(l) {
					c.Log("Language '%s' is already installed.", l)
				} else {
					c.Log("Downloading %s…", l)
					if err := mgr.Install(c.Context, in, l, nil); err != nil {
						return cli.Errorf("installing %s: %v", l, err)
					}
				}
				// The plugin packs, synchronously: the admin downloads them in a goroutine that would
				// die with this process.
				c.Log("Downloading the plugin translations of %s…", l)
				if err := mgr.Install(c.Context, in, l, langPluginPacks); err != nil {
					c.Warning("plugin translations of %s: %v", l, err)
				}
				c.Success("Language '%s' installed.", l)
			}
			if c.Bool("activate") {
				return opsActivateLanguage(c, e, c.Args[len(c.Args)-1])
			}
			return nil
		},
	})

	r.Add(cli.Command{
		Path:  "language core activate",
		Short: "Make an installed language the site language.",
		Long: "Sets the WPLANG option (Settings › General › Site Language; en_US clears it), rebuilds the search\n" +
			"stems for the new language and purges the page cache of running servers. A running server that\n" +
			"already loaded this language before its pack was installed needs a restart to see it.",
		Args:     []cli.Arg{{Name: "language", Usage: "Locale, e.g. de_DE, or en_US."}},
		Examples: []string{"language core activate de_DE", "language core activate en_US"},
		Run: func(c *cli.Ctx) error {
			return opsActivateLanguage(c, e, c.Arg(0))
		},
	})

	r.Add(cli.Command{
		Path:     "language core uninstall",
		Short:    "Delete the files of installed language packs.",
		Long:     "Removes a locale's core pack, script translations and plugin translations from the languages\ndirectory. The active language cannot be removed.",
		Args:     []cli.Arg{{Name: "language", Usage: "Locale.", Repeat: true}},
		Examples: []string{"language core uninstall de_DE --yes"},
		Run: func(c *cli.Ctx) error {
			set, err := e.Settings(c.Context)
			if err != nil {
				return err
			}
			active := defaultStr(set.String("WPLANG"), "en_US")
			dir := e.langDir()
			files := map[string][]string{}
			total := 0
			for _, l := range c.Args {
				switch {
				case l == "en_US":
					return cli.Errorf("en_US is built in and cannot be uninstalled")
				case l == active:
					return cli.Errorf("'%s' is the site language: activate another one first", l)
				case !validLanguage(l):
					return cli.Errorf("invalid language '%s'", l)
				}
				files[l] = langFiles(dir, l)
				if len(files[l]) == 0 {
					return cli.Errorf("language '%s' is not installed", l)
				}
				total += len(files[l])
			}
			if err := c.Confirm("Delete %d file(s) of %s?", total, strings.Join(c.Args, ", ")); err != nil {
				return err
			}
			for _, l := range c.Args {
				for _, f := range files[l] {
					if err := os.Remove(filepath.Join(dir, filepath.FromSlash(f))); err != nil && !os.IsNotExist(err) {
						return err
					}
				}
				c.Success("Language '%s' uninstalled.", l)
			}
			return nil
		},
	})
}

// opsActivateLanguage is Settings › General's Site Language save: WPLANG, audit, page cache purge
// and the search stem rebuild (which the admin runs in the background, here synchronously).
func opsActivateLanguage(c *cli.Ctx, e *cliEnv, locale string) error {
	if locale == "" || !validLanguage(locale) {
		return cli.Errorf("invalid language '%s' (see %s language core list)", locale, c.Prog)
	}
	value := locale
	if value == "en_US" {
		value = ""
	}
	if !i18n.NewManager(e.langDir(), nil).IsInstalled(locale) {
		return cli.Errorf("language '%s' is not installed; run '%s language core install %s' first", locale, c.Prog, locale)
	}
	s, err := e.Server(c.Context)
	if err != nil {
		return err
	}
	if s.settings.String("WPLANG") == value {
		c.Success("Language '%s' is already the site language.", locale)
		return nil
	}
	if err := s.settings.Set(c.Context, map[string]any{"WPLANG": value}); err != nil {
		return err
	}
	s.asAudit(c.Context, opsAuditEvent(c, e, asEvent{Event: "settings_updated", Severity: "notice", Object: "general", Message: "Site language set to " + locale + " (command line)."}))
	if err := e.PurgeAll(c.Context); err != nil {
		c.Warning("page cache purge: %v", err)
	}
	c.Log("Rebuilding the search stems for %s…", locale)
	if _, err := s.store.SyncSearchStem(c.Context, value); err != nil {
		c.Warning("search stemming: %v", err)
	}
	c.Warning("a running server that already loaded '%s' before it was installed shows it after a restart.", locale)
	c.Success("Language activated: %s", locale)
	return nil
}

// opsAuditEvent fills the acting user of an audit row from --user (else "cli"): a command has no
// request to take the user and IP from.
func opsAuditEvent(c *cli.Ctx, e *cliEnv, ev asEvent) asEvent {
	if c.Flag("user") != "" {
		if u, err := e.Actor(c); err == nil {
			ev.User = u
			return ev
		}
	}
	ev.Username = "cli"
	return ev
}

// --- mail ---------------------------------------------------------------------------

func registerOpsMailCLI(r *cli.Registry, e *cliEnv) {
	r.Noun("mail", "Test e-mail delivery.")

	r.Add(cli.Command{
		Path:  "mail test",
		Short: "Send a test e-mail through the site's mail transport.",
		Long: "Sends a message synchronously through the transport wp_mail uses: Pursae SMTP's connection when\n" +
			"the plugin is active (the attempt is written to its Email Log and debug events, and Do Not Send\n" +
			"is respected), else PURSAE_SMTP_HOST. Without either the message is only saved as an .eml file in\n" +
			"PURSAE_MAIL_DIR and not delivered.",
		Args: []cli.Arg{{Name: "to", Usage: "Recipient address."}},
		Flags: []cli.Flag{
			{Name: "html", Bool: true, Usage: "Send an HTML message (Pursae SMTP)."},
			{Name: "connection", Usage: "Pursae SMTP connection: primary or a backup connection's ID.", Default: "primary"},
			{Name: "debug", Bool: true, Usage: "Print the transport transcript."},
		},
		Examples: []string{"mail test admin@example.com", "mail test admin@example.com --html --debug"},
		Run: func(c *cli.Ctx) error {
			to := strings.TrimSpace(c.Arg(0))
			if _, err := parseOneAddr(to); err != nil {
				return cli.Usagef("invalid e-mail address '%s'", to)
			}
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			if s.mailSMTPOn() {
				res := s.msTest(c.Context, s.msLoad(), to, c.Flag("connection"), c.Bool("html"))
				if c.Bool("debug") && res.Debug != "" {
					fmt.Fprintln(c.Stderr(), strings.TrimRight(res.Debug, "\n"))
				}
				if !res.OK {
					for _, h := range res.Hints {
						c.Warning("%s", h)
					}
					return cli.Errorf("%s", res.Message)
				}
				c.Success("Test e-mail sent to %s through %s (%s connection).", to, res.Mailer, res.Conn)
				return nil
			}
			if c.Bool("html") || c.Has("connection") {
				c.Warning("--html and --connection apply to Pursae SMTP, which is inactive.")
			}
			m := s.mailer()
			site := s.settings.String("blogname")
			msg := mail.Message{To: []string{to}, Subject: "Pursae: Test Email to " + to,
				Body: "This is a test email from " + site + " (" + s.settings.String("home") + "), sent with `" + c.Prog + " mail test`.\n\nIf you can read this, email delivery works."}
			if err := m.Send(c.Context, msg); err != nil {
				return cli.Errorf("sending failed: %v", err)
			}
			if !m.Configured() && m.Transport == nil {
				c.Warning("no SMTP server is configured (PURSAE_SMTP_HOST) and Pursae SMTP is inactive: the message was saved as an .eml file in %s, not delivered.", m.Dir)
				return nil
			}
			c.Success("Test e-mail sent to %s through %s:%s.", to, m.Host, m.Port)
			return nil
		},
	})
}

// --- site ---------------------------------------------------------------------------

func registerOpsSiteCLI(r *cli.Registry, e *cliEnv) {
	r.Noun("site", "Site Health status and information.")

	r.Add(cli.Command{
		Path:  "site health",
		Short: "Run the Site Health checks.",
		Long: "Runs the checks of Tools › Site Health › Status and prints them with the overall score. Exits\n" +
			"with 1 when a check is critical. Checks about the server process itself (the plugin sandbox,\n" +
			"live page cache statistics) are not available from the command line.",
		Flags: []cli.Flag{
			{Name: "status", Usage: "Only checks with this status.", Values: []string{"critical", "recommended", "good"}},
		},
		Examples: []string{"site health", "site health --status=critical --format=json"},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			tests := s.healthTests(c.Context)
			if t, ok := s.msHealthTest(c.Context); ok {
				tests = append(tests, t)
			}
			tests = cliHealthTests(tests)
			counts := map[string]int{}
			var out []cli.Row
			for _, t := range tests {
				st := t.Status
				if st != "good" && st != "recommended" {
					st = "critical"
				}
				counts[st]++
				if f := c.Flag("status"); f != "" && f != st {
					continue
				}
				out = append(out, cli.Row{"test": healthSlug(t.Label), "label": t.Label, "status": st, "badge": t.Badge,
					"description": t.Description, "action": healthActionText(string(t.Action))})
			}
			// Critical first, like the screen.
			rank := map[string]int{"critical": 0, "recommended": 1, "good": 2}
			slices.SortStableFunc(out, func(a, b cli.Row) int { return rank[a["status"].(string)] - rank[b["status"].(string)] })
			if err := c.Print(out, cli.View{Fields: []string{"status", "badge", "label"}, ID: "test"}); err != nil {
				return err
			}
			score, label := healthScore(counts["good"], counts["recommended"], counts["critical"])
			if c.Format == "table" && c.Field == "" {
				c.Log("\nScore: %d%% (%s): %d critical, %d recommended, %d good.", score, label, counts["critical"], counts["recommended"], counts["good"])
			}
			if counts["critical"] > 0 {
				return cli.Exit(cli.ExitFailure)
			}
			return nil
		},
	})

	r.Add(cli.Command{
		Path:  "site info",
		Short: "Print the Site Health information (versions, sizes, schedule, mail).",
		Long: "Prints Tools › Site Health › Info. Values that describe the server process (memory, goroutines)\n" +
			"are left out: this command runs in its own process.",
		Flags:    []cli.Flag{{Name: "section", Usage: "Only this section (e.g. database, server, scheduled-events)."}},
		Examples: []string{"site info", "site info --section=database", "site info --format=json"},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			s.loadThemePackages() // the active theme may be a package
			var active []string
			if rows, err := s.store.Pool.Query(c.Context, `SELECT slug || ' ' || version FROM plugins WHERE status = 'active' ORDER BY slug`); err == nil {
				for rows.Next() {
					var v string
					if rows.Scan(&v) == nil {
						active = append(active, v)
					}
				}
				rows.Close()
			}
			want := c.Flag("section")
			var out []cli.Row
			var slugs []string
			for _, sec := range s.healthInfo(c.Context) {
				slug := healthSlug(sec.Title)
				slugs = append(slugs, slug)
				if want != "" && want != slug && !strings.EqualFold(want, sec.Title) {
					continue
				}
				for _, f := range cliHealthInfoFields(sec.Title, sec.Fields, active) {
					out = append(out, cli.Row{"section": slug, "field": f[0], "value": f[1]})
				}
			}
			if want != "" && len(out) == 0 {
				return cli.Errorf("no section '%s'; sections: %s", want, strings.Join(slugs, ", "))
			}
			return c.Print(out, cli.View{Fields: []string{"section", "field", "value"}, ID: "field"})
		},
	})
}

// cliHealthTests adapts the Site Health checks to a command-line process: it has no plugin
// runtime (the sandbox check would always read critical) and its page cache only forwards purges
// (its statistics are not the server's).
func cliHealthTests(tests []healthTest) []healthTest {
	out := tests[:0:0]
	for _, t := range tests {
		switch t.Label {
		case healthLabelSandboxUp, healthLabelSandboxDown:
			continue
		case healthLabelCacheOn:
			t.Description = "Pursae Cache serves pages from memory. Hit statistics are only available in the running server (Pursae Cache screen)."
		}
		out = append(out, t)
	}
	return out
}

// cliHealthInfoFields drops the Info values that describe the CLI process instead of the server,
// and lists the active Wasm plugins from the database (the CLI does not load them).
func cliHealthInfoFields(section string, fields [][2]string, activePlugins []string) [][2]string {
	var out [][2]string
	for _, f := range fields {
		switch {
		case section == "Server" && (f[0] == "Goroutines" || f[0] == "Memory in use"):
			continue
		case section == "Plugins" && f[0] == "Active Wasm plugins":
			f[1] = defaultStr(strings.Join(activePlugins, ", "), "None")
		}
		out = append(out, f)
	}
	return out
}

// healthScore is the Site Health progress circle (site-health.js recalculateProgression) and its label.
func healthScore(good, rec, crit int) (int, string) {
	total := float64(good) + float64(rec) + 1.5*float64(crit)
	failed := 0.5*float64(rec) + 1.5*float64(crit)
	val := 100
	if total > 0 {
		val = 100 - int(math.Ceil(failed/total*100))
	}
	val = max(0, min(100, val))
	if val >= 80 && crit == 0 {
		return val, "Good"
	}
	return val, "Should be improved"
}

var hrefRe = regexp.MustCompile(`(?i)href\s*=\s*"([^"]*)"`)

// healthActionText turns a check's action HTML into text: the link text with its target.
func healthActionText(h string) string {
	if h == "" {
		return ""
	}
	text := strings.TrimSpace(html.UnescapeString(stripTagsSimple(h)))
	if m := hrefRe.FindStringSubmatch(h); m != nil {
		return strings.TrimSpace(text + " (" + html.UnescapeString(m[1]) + ")")
	}
	return text
}
