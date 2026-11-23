package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"wpgo/internal/auth"
	"wpgo/internal/config"
	"wpgo/internal/settings"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// The WP to Pursae importer: a package (wp2p_package.go) becomes this site's users, content, media,
// menus, settings and Pursae Commerce data. The run goes phase by phase (extract, users, content,
// options, tax rates, coupons, orders, customers, finish) and records each finished phase and the
// report in its wp2p_migrations row, so a run that stops (a restart, a failure) resumes where it was:
// finished phases are skipped, and inside a phase every record is looked up in wp2p_map before it is
// created, so running it again never duplicates anything. Content goes through the WXR importer
// (importWXRReader) with the package's media files instead of downloads.

// wp2pPhases are the phases in order.
var wp2pPhases = []string{"extract", "users", "content", "options", "tax_rates", "coupons", "orders", "customers", "finish"}

// wp2pPhaseLabels name the phases on the progress screen and in the CLI.
var wp2pPhaseLabels = map[string]string{
	"extract": "Opening the package", "users": "Users", "content": "Content, media and menus", "options": "Settings",
	"tax_rates": "Tax rates", "coupons": "Coupons", "orders": "Orders and refunds", "customers": "Customers", "finish": "Finishing",
}

// wp2pProgress is the run's state in wp2p_migrations.progress.
type wp2pProgress struct {
	Phase string         `json:"phase"`
	Done  []string       `json:"done"`  // finished phases
	Items map[string]int `json:"items"` // records handled in the current phase
}

func (p *wp2pProgress) finished(phase string) bool {
	for _, d := range p.Done {
		if d == phase {
			return true
		}
	}
	return false
}

// wp2pReport is the final report (wp2p_migrations.report).
type wp2pReport struct {
	Source   string         `json:"source"`
	Counts   map[string]int `json:"counts"`   // what was imported, per kind
	Existing map[string]int `json:"existing"` // what was already here (merged or skipped), per kind
	Skipped  []wp2pSkip     `json:"skipped"`  // what was not imported, and why
	Unmapped map[string]int `json:"unmapped"` // WordPress fields with no Pursae equivalent: how many records had them
	Notes    []string       `json:"notes"`
	Content  *importReport  `json:"content,omitempty"`
}

func newWP2PReport() *wp2pReport {
	return &wp2pReport{Counts: map[string]int{}, Existing: map[string]int{}, Unmapped: map[string]int{}}
}

// wp2pMaxSkips bounds the itemized skip list (counts stay exact).
const wp2pMaxSkips = 500

func (r *wp2pReport) skip(kind, item, reason string) {
	r.Counts["skipped_"+kind]++
	if len(r.Skipped) < wp2pMaxSkips {
		r.Skipped = append(r.Skipped, wp2pSkip{Kind: kind, Item: item, Reason: reason})
	}
}

func (r *wp2pReport) note(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	for _, n := range r.Notes {
		if n == line {
			return
		}
	}
	r.Notes = append(r.Notes, line)
}

// wp2pRun is one run of the importer.
type wp2pRun struct {
	s   *Server
	id  string // wp2p_migrations.id
	dir string // the extracted package
	pkg string // the package file
	// keepPackage leaves the package file in place once it is opened (the CLI's own file); uploads are removed.
	keepPackage bool
	key         *wp2pKey
	u           *store.User // the importing administrator
	src         string      // source site, the wp2p_map namespace
	man         *wp2pManifest
	prog        *wp2pProgress
	rep         *wp2pReport
	logf        func(format string, args ...any) // progress lines (CLI); nil = none

	users          map[int64]int64  // WordPress user id -> ours
	logins         map[string]int64 // WordPress login -> our user id
	posts, terms   map[int64]int64  // WordPress post / term ids -> ours (from the content phase)
	taxRates       map[int64]int64
	coupons        map[int64]int64
	couponByCode   map[string]int64
	lastProgressAt time.Time
}

// wp2pImportDir keeps packages and their extracted files until the import is over. Like the WXR
// importer's directory it is outside the uploads directory, which the web server serves.
func (s *Server) wp2pImportDir() string {
	return filepath.Join(filepath.Dir(filepath.Clean(s.cfg.UploadsDir)), "wp2p-imports")
}

func (s *Server) wp2pPackagePath(id string) string {
	return filepath.Join(s.wp2pImportDir(), id+".wp2p")
}
func (s *Server) wp2pWorkDir(id string) string { return filepath.Join(s.wp2pImportDir(), id) }

func (r *wp2pRun) log(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

// save writes the progress and the report to the run's row (every few seconds while a phase runs).
func (r *wp2pRun) save(force bool) {
	if !force && time.Since(r.lastProgressAt) < 2*time.Second {
		return
	}
	r.lastProgressAt = time.Now()
	prog, _ := json.Marshal(r.prog)
	rep, _ := json.Marshal(r.rep)
	_, _ = r.s.store.Pool.Exec(context.Background(), `UPDATE wp2p_migrations SET progress = $2, report = $3, source = $4, updated_at = now() WHERE id = $1`,
		r.id, prog, rep, r.src)
}

// mapGet / mapSet read and write wp2p_map.
func (r *wp2pRun) mapGet(ctx context.Context, kind string, old int64) int64 {
	var id int64
	_ = r.s.store.Pool.QueryRow(ctx, `SELECT new_id FROM wp2p_map WHERE source = $1 AND kind = $2 AND old_id = $3`, r.src, kind, old).Scan(&id)
	return id
}

func (r *wp2pRun) mapSet(ctx context.Context, kind string, old, id int64) error {
	_, err := r.s.store.Pool.Exec(ctx, `INSERT INTO wp2p_map (source, kind, old_id, new_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (source, kind, old_id) DO UPDATE SET new_id = EXCLUDED.new_id`, r.src, kind, old, id)
	return err
}

func (r *wp2pRun) mapSetAll(ctx context.Context, kind string, m map[int64]int64) error {
	if len(m) == 0 {
		return nil
	}
	olds := make([]int64, 0, len(m))
	news := make([]int64, 0, len(m))
	for k, v := range m {
		olds, news = append(olds, k), append(news, v)
	}
	_, err := r.s.store.Pool.Exec(ctx, `INSERT INTO wp2p_map (source, kind, old_id, new_id) SELECT $1, $2, o, n FROM unnest($3::bigint[], $4::bigint[]) AS t(o, n)
		ON CONFLICT (source, kind, old_id) DO UPDATE SET new_id = EXCLUDED.new_id`, r.src, kind, olds, news)
	return err
}

func (r *wp2pRun) mapLoad(ctx context.Context, kind string) map[int64]int64 {
	out := map[int64]int64{}
	rows, err := r.s.store.Pool.Query(ctx, `SELECT old_id, new_id FROM wp2p_map WHERE source = $1 AND kind = $2`, r.src, kind)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var o, n int64
		if rows.Scan(&o, &n) == nil {
			out[o] = n
		}
	}
	return out
}

// wp2pRecordHook, when set (tests), is called for every record a phase handles, with the phase's
// item kind.
var wp2pRecordHook func(kind string)

// item counts a record of the current phase.
func (r *wp2pRun) item(kind string) {
	r.prog.Items[kind]++
	if wp2pRecordHook != nil {
		wp2pRecordHook(kind)
	}
}

// wp2pStopped reports whether err, met while writing one record, means the run must stop instead of
// reporting the record as skipped and going on: the job's context ended (a shutdown, a restart) or
// the database cannot be reached. A phase that skipped its remaining records for that reason would be
// recorded as finished and never run again.
func wp2pStopped(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ce *pgconn.ConnectError
	return errors.As(err, &ce) || pgconn.Timeout(err)
}

// wp2pSkipReason is why a record is not imported (reported, not fatal).
type wp2pSkipReason string

func (e wp2pSkipReason) Error() string { return string(e) }

// wp2pUserRef names a WordPress user in the report by id, not by login or e-mail address: the report
// is kept and shown, and must not become a list of personal data.
func wp2pUserRef(id int64) string { return "#" + strconv.FormatInt(id, 10) }

// wp2pSource is the wp2p_map namespace of a site: its home URL without scheme and trailing slash.
func wp2pSource(m *wp2pManifest) string {
	u := strings.TrimSpace(m.HomeURL)
	if u == "" {
		u = m.SiteURL
	}
	u = strings.ToLower(strings.TrimRight(u, "/"))
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return u
}

// run goes through the phases that are not finished yet.
func (r *wp2pRun) run(ctx context.Context) error {
	if r.prog == nil {
		r.prog = &wp2pProgress{}
	}
	if r.rep == nil {
		r.rep = newWP2PReport()
	}
	for _, phase := range wp2pPhases {
		if r.prog.finished(phase) {
			if phase == "extract" && r.man == nil {
				m, err := wp2pReadManifest(filepath.Join(r.dir, "manifest.json"))
				if err != nil {
					// The extracted files are gone (cleaned up, another machine): open the package again.
					r.prog.Done = nil
					return r.run(ctx)
				}
				r.man = m
				r.src = wp2pSource(m)
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		r.prog.Phase, r.prog.Items = phase, map[string]int{}
		r.save(true)
		r.log("%s …", wp2pPhaseLabels[phase])
		var err error
		switch phase {
		case "extract":
			err = r.extract(ctx)
		case "users":
			err = r.importUsers(ctx)
		case "content":
			err = r.importContent(ctx)
		case "options":
			err = r.importOptions(ctx)
		case "tax_rates":
			err = r.importTaxRates(ctx)
		case "coupons":
			err = r.importCoupons(ctx)
		case "orders":
			err = r.importOrders(ctx)
		case "customers":
			err = r.importCustomers(ctx)
		case "finish":
			err = r.finish(ctx)
		}
		if err == nil {
			// A phase whose records stopped coming because the run is being stopped is not finished,
			// whatever it returned: the next run does it again (every record is looked up first).
			err = ctx.Err()
		}
		if err != nil {
			r.save(true)
			return fmt.Errorf("%s: %w", strings.ToLower(wp2pPhaseLabels[phase]), err)
		}
		r.prog.Done = append(r.prog.Done, phase)
		r.save(true)
	}
	return nil
}

// extract opens the package into the work directory and forgets the key.
func (r *wp2pRun) extract(ctx context.Context) error {
	if r.key == nil {
		return errors.New("the package key is missing; start the import again")
	}
	_ = os.RemoveAll(r.dir)
	man, ignored, err := wp2pExtract(r.pkg, *r.key, r.dir)
	if err != nil {
		return err
	}
	r.man, r.src = man, wp2pSource(man)
	r.rep.Source = man.HomeURL
	for _, sk := range man.Skipped {
		r.rep.skip(sk.Kind, sk.Item, sk.Reason)
	}
	if len(ignored) > 0 {
		r.rep.note("%d unknown entries of the package were ignored (a newer plugin?).", len(ignored))
	}
	// The plaintext is on disk now (0600, outside the web root): the key is not needed any more.
	_, _ = r.s.store.Pool.Exec(ctx, `UPDATE wp2p_migrations SET package_key = NULL, source = $2 WHERE id = $1`, r.id, r.src)
	if !r.keepPackage {
		_ = os.Remove(r.pkg)
	}
	return nil
}

// wp2pUser is a line of users.jsonl.
type wp2pUser struct {
	ID          int64             `json:"id"`
	Login       string            `json:"login"`
	Email       string            `json:"email"`
	Pass        string            `json:"pass"`
	Nicename    string            `json:"nicename"`
	DisplayName string            `json:"display_name"`
	URL         string            `json:"url"`
	Registered  string            `json:"registered"` // user_registered (GMT)
	Roles       []string          `json:"roles"`
	Meta        map[string]string `json:"meta"`
}

// wp2pProfileKeys are the user meta that are Pursae profile fields.
var wp2pProfileKeys = []string{"first_name", "last_name", "nickname", "description"}

// wp2pUserMetaKept are the other user meta keys that are kept as user meta.
func wp2pUserMetaKept(k string) bool {
	return strings.HasPrefix(k, "billing_") || strings.HasPrefix(k, "shipping_") || k == "paying_customer"
}

// importUsers creates the users with their WordPress password hashes. A user whose e-mail address
// is already here is that account (it is not changed: its password stays) and their content is
// assigned to it. A login that is taken here by an account with another e-mail address is another
// person: the user is created under a free login (login-2) instead of being merged.
func (r *wp2pRun) importUsers(ctx context.Context) error {
	r.users, r.logins = map[int64]int64{}, map[string]int64{}
	roles, err := r.s.store.Roles(ctx)
	if err != nil {
		return err
	}
	unknownRoles := map[string]int{}
	line := 0
	err = wp2pEachLine(ctx, filepath.Join(r.dir, "users.jsonl"), func(raw []byte) error {
		line++
		var wu wp2pUser
		if json.Unmarshal(raw, &wu) != nil || wu.ID == 0 || wu.Login == "" {
			// The line number only: the raw record holds an e-mail address and a password hash.
			r.rep.skip("user", "users.jsonl line "+strconv.Itoa(line), "unreadable line")
			return nil //nolint:nilerr // a bad record is reported and skipped, not fatal
		}
		r.item("users")
		if id := r.mapGet(ctx, "user", wu.ID); id != 0 {
			if _, err := r.s.store.UserByID(ctx, id); err == nil {
				r.users[wu.ID], r.logins[wu.Login] = id, id
				r.rep.Existing["users"]++
				return nil
			}
		}
		id, how, err := r.importUser(ctx, &wu, roles, unknownRoles)
		if err != nil {
			if wp2pStopped(ctx, err) {
				return err
			}
			r.rep.skip("user", wp2pUserRef(wu.ID), err.Error())
			return nil
		}
		r.users[wu.ID], r.logins[wu.Login] = id, id
		switch how {
		case wp2pUserMerged:
			r.rep.Existing["users"]++
			r.rep.Counts["users_merged_by_email"]++
		case wp2pUserRenamed:
			r.rep.Counts["users"]++
			r.rep.Counts["users_renamed"]++
		default:
			r.rep.Counts["users"]++
		}
		r.save(false)
		return nil
	})
	if err != nil {
		return err
	}
	for role, n := range unknownRoles {
		r.rep.note("The role “%s” does not exist here: %d user(s) got the role Subscriber instead.", role, n)
	}
	if n := r.rep.Counts["users_merged_by_email"]; n > 0 {
		r.rep.note("%d user(s) have an account here with the same e-mail address: their content and orders were assigned to that account, whose password and profile were not changed.", n)
	}
	if n := r.rep.Counts["users_renamed"]; n > 0 {
		r.rep.note("%d user(s) had a login that an account here already uses with another e-mail address: they were imported under a new login (with -2, -3 … added) and sign in with it or with their e-mail address.", n)
	}
	return nil
}

// How importUser placed a user.
const (
	wp2pUserCreated = iota
	wp2pUserMerged  // an account here with the same e-mail address
	wp2pUserRenamed // created under another login: the login is another person's here
)

// importUser creates one user, or finds the account that is already here (the same e-mail address).
// The user, its roles, dates, meta and its wp2p_map row are written in one transaction, so a run that
// stops never leaves a half-made user that the next run would take for an account of this site.
// A wp2pSkipReason error means the user is skipped.
func (r *wp2pRun) importUser(ctx context.Context, wu *wp2pUser, roles []store.Role, unknownRoles map[string]int) (int64, int, error) {
	st := r.s.store
	login, email := strings.TrimSpace(wu.Login), strings.TrimSpace(wu.Email)
	if email != "" {
		var id int64
		err := st.Pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = lower($1) ORDER BY id LIMIT 1`, email).Scan(&id)
		if err == nil {
			return id, wp2pUserMerged, r.mapSet(ctx, "user", wu.ID, id)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, err
		}
	}
	if !validUsername.MatchString(login) || len(login) > 60 {
		return 0, 0, wp2pSkipReason("the login has characters Pursae does not allow in user names")
	}
	how := wp2pUserCreated
	free, err := r.freeLogin(ctx, login)
	if err != nil {
		return 0, 0, err
	}
	if free != login {
		login, how = free, wp2pUserRenamed
	}
	hash := strings.TrimSpace(wu.Pass)
	resetNeeded := false
	if !auth.IsImportableHash(hash) {
		h, err := auth.HashPassword(auth.RandomToken(24))
		if err != nil {
			return 0, 0, err
		}
		hash, resetNeeded = h, true
	}
	primary, extra := "", []string{}
	for _, role := range wu.Roles {
		if !roleListed(roles, role) {
			unknownRoles[role]++
			continue
		}
		if primary == "" {
			primary = role
		} else {
			extra = append(extra, role)
		}
	}
	if primary == "" {
		primary = "subscriber"
		if len(wu.Roles) == 0 {
			primary = ""
		}
	}
	profile := map[string]string{}
	for _, k := range wp2pProfileKeys {
		if v, ok := wu.Meta[k]; ok {
			profile[k] = v
		}
	}
	nu := store.NewUser{Login: login, Email: email, PasswordHash: hash, DisplayName: defaultStr(strings.TrimSpace(wu.DisplayName), login), Role: primary, Profile: profile}
	if primary == "" {
		// A user without a role on the WordPress site (wp_capabilities empty) stays without one.
		nu.Role = "subscriber"
	}
	reg := time.Now().UTC()
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", wu.Registered, time.UTC); err == nil && t.Year() > 1970 {
		reg = t
	}
	meta := map[string]any{}
	for k, v := range wu.Meta {
		if wp2pUserMetaKept(k) && v != "" {
			meta[k] = v
		}
	}
	var id int64
	err = pgx.BeginFunc(ctx, st.Pool, func(tx pgx.Tx) error {
		var err error
		if id, err = st.CreateUserTx(ctx, tx, nu); err != nil {
			return err
		}
		if primary == "" || len(extra) > 0 {
			if err := store.SetUserRolesTx(ctx, tx, id, primary, extra); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET registered_at = $2, url = $3, locale = $4 WHERE id = $1`, id, reg, strings.TrimSpace(wu.URL), wu.Meta["locale"]); err != nil {
			return err
		}
		// WordPress' nicename keeps the author archive URL (/author/<nicename>/) when it is free here.
		if nn := wp.SanitizeTitle(wu.Nicename); nn != "" {
			if _, err := tx.Exec(ctx, `UPDATE users SET nicename = $2 WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM users WHERE nicename = $2 AND id <> $1)`, id, nn); err != nil {
				return err
			}
		}
		if len(meta) > 0 {
			raw, _ := json.Marshal(meta)
			if _, err := tx.Exec(ctx, `UPDATE users SET meta = coalesce(meta, '{}'::jsonb) || $2::jsonb WHERE id = $1`, id, raw); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO wp2p_map (source, kind, old_id, new_id) VALUES ($1, 'user', $2, $3)
			ON CONFLICT (source, kind, old_id) DO UPDATE SET new_id = EXCLUDED.new_id`, r.src, wu.ID, id)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrLoginTaken) || errors.Is(err, store.ErrEmailTaken) {
			return 0, 0, wp2pSkipReason(err.Error())
		}
		return 0, 0, err
	}
	if resetNeeded {
		r.rep.Counts["users_password_reset"]++
	}
	return id, how, nil
}

// freeLogin is login when no account here has it, else the first free login-2, login-3 … (at most 60
// characters, as user names are).
func (r *wp2pRun) freeLogin(ctx context.Context, login string) (string, error) {
	cand := login
	for i := 2; ; i++ {
		var taken bool
		if err := r.s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(login) = lower($1))`, cand).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return cand, nil
		}
		suffix := "-" + strconv.Itoa(i)
		base := login
		if len(base)+len(suffix) > 60 {
			base = base[:60-len(suffix)]
		}
		cand = base + suffix
	}
}

// loadUsers rebuilds the user maps when the users phase ran in an earlier attempt.
func (r *wp2pRun) loadUsers(ctx context.Context) error {
	if r.users != nil {
		return nil
	}
	r.users, r.logins = r.mapLoad(ctx, "user"), map[string]int64{}
	return wp2pEachLine(ctx, filepath.Join(r.dir, "users.jsonl"), func(raw []byte) error {
		var wu wp2pUser
		if json.Unmarshal(raw, &wu) == nil {
			if id, ok := r.users[wu.ID]; ok {
				r.logins[wu.Login] = id
			}
		}
		return nil
	})
}

// wp2pMedia is a line of media.jsonl.
type wp2pMedia struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// importContent runs the WXR importer over content.xml with the package's media files.
func (r *wp2pRun) importContent(ctx context.Context) error {
	if err := r.loadUsers(ctx); err != nil {
		return err
	}
	// WXR dates are in the site's time zone: take WordPress' before reading them (a post published an
	// hour ago in Berlin must not become scheduled because this site still runs on UTC).
	if err := r.importTimezone(ctx); err != nil {
		return err
	}
	files := map[int64]wp2pMedia{}
	if err := wp2pEachLine(ctx, filepath.Join(r.dir, "media.jsonl"), func(raw []byte) error {
		var m wp2pMedia
		if json.Unmarshal(raw, &m) == nil && m.ID > 0 {
			files[m.ID] = m
		}
		return nil
	}); err != nil {
		return err
	}
	contentPath := filepath.Join(r.dir, "content.xml")
	if size, limit := wp2pSize(contentPath), wp2pMaxContent(); size > limit {
		// The WXR importer reads the whole file into memory before it imports anything.
		return fmt.Errorf("content.xml is %d MB, more than this site imports in one go (%d MB): raise PURSAE_WP2P_MAX_CONTENT_MB where the server has the memory, "+
			"or build the package in parts (one post type at a time)", size>>20, limit>>20)
	}
	f, err := wp2pOpen(contentPath)
	if errors.Is(err, os.ErrNotExist) {
		r.rep.note("The package has no content (content.xml).")
		r.posts, r.terms = map[int64]int64{}, map[int64]int64{}
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	authors := map[string]wxrAuthorChoice{}
	for login, id := range r.logins {
		authors[login] = wxrAuthorChoice{UserID: id}
	}
	known := r.mapLoad(ctx, "post")
	r.posts, r.terms = map[int64]int64{}, map[int64]int64{}
	opts := wxrImportOptions{Known: known, Authors: authors, FetchAttachments: true, IDMap: r.posts, TermMap: r.terms, MenuItemsOnce: true, WooElsewhere: true,
		// Every id goes to wp2p_map as soon as it is known: a run that stops halfway resumes with
		// Known holding everything it created (drafts without a slug included), not a second copy.
		OnMap: func(kind string, old, id int64) error {
			r.item("content")
			if err := r.mapSet(ctx, kind, old, id); err != nil {
				return err
			}
			r.save(false)
			return nil
		},
		LocalFile: func(postID int64) (string, string, bool) {
			m, ok := files[postID]
			if !ok {
				return "", "", false
			}
			p := filepath.Join(r.dir, "media", strconv.FormatInt(postID, 10))
			if _, err := os.Stat(p); err != nil {
				return "", "", false
			}
			return p, m.Name, true
		}}
	rep, err := r.s.importWXRReader(ctx, r.u, f, opts)
	if err != nil {
		return err
	}
	r.rep.Content = rep
	for k, v := range map[string]int{"posts": rep.Posts, "pages": rep.Pages, "patterns": rep.Blocks, "templates": rep.Templates, "terms": rep.Terms,
		"comments": rep.Comments, "media": rep.Attachments, "menu_items": rep.MenuItems, "products": rep.Products, "variations": rep.Variations} {
		r.rep.Counts[k] += v
	}
	if err := r.mapSetAll(ctx, "post", r.posts); err != nil {
		return err
	}
	return r.mapSetAll(ctx, "term", r.terms)
}

func (r *wp2pRun) loadContentMaps(ctx context.Context) {
	if r.posts == nil {
		r.posts = r.mapLoad(ctx, "post")
	}
	if r.terms == nil {
		r.terms = r.mapLoad(ctx, "term")
	}
}

// wp2pMaxContent is the largest content.xml imported (PURSAE_WP2P_MAX_CONTENT_MB, default 2048 MB): the
// WXR importer holds the whole file in memory.
func wp2pMaxContent() int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(config.Env("WP2P_MAX_CONTENT_MB")), 10, 64); err == nil && n > 0 {
		return n << 20
	}
	return 2048 << 20
}

// importTimezone sets timezone_string and gmt_offset from options.json (the options phase sets them
// again, with the rest).
func (r *wp2pRun) importTimezone(ctx context.Context) error {
	raw, _ := os.ReadFile(filepath.Join(r.dir, "options.json")) // none: nothing to set
	var o wp2pOptions
	if len(raw) == 0 || json.Unmarshal(raw, &o) != nil {
		return nil //nolint:nilerr // no usable options.json: nothing to set
	}
	vals := map[string]any{}
	if v, ok := o.Options["timezone_string"].(string); ok {
		vals["timezone_string"] = v
	}
	if v, ok := wp2pOptionValue(o.Options["gmt_offset"], float64(0)); ok && o.Options["gmt_offset"] != nil {
		vals["gmt_offset"] = v
	}
	if len(vals) == 0 {
		return nil
	}
	return r.s.settings.Set(ctx, vals)
}

// wp2pOptions is options.json.
type wp2pOptions struct {
	Options       map[string]any    `json:"options"`
	MenuLocations map[string]string `json:"menu_locations"` // theme location -> menu slug
	Theme         string            `json:"theme"`
	Plugins       []string          `json:"active_plugins"`
}

// wp2pCoreOptions are the WordPress options that move; values that name posts or terms are mapped.
var wp2pCoreOptions = []string{"blogname", "blogdescription", "permalink_structure", "category_base", "tag_base", "timezone_string",
	"gmt_offset", "date_format", "time_format", "start_of_week", "posts_per_page", "posts_per_rss", "rss_use_excerpt", "show_on_front",
	"page_on_front", "page_for_posts", "wp_page_for_privacy_policy", "default_category", "default_comment_status", "default_ping_status",
	"comment_registration", "close_comments_for_old_posts", "close_comments_days_old", "thread_comments", "thread_comments_depth",
	"page_comments", "comments_per_page", "default_comments_page", "comment_order", "comment_moderation", "comment_previously_approved",
	"require_name_email", "show_avatars", "avatar_rating", "avatar_default", "blog_public", "users_can_register", "default_role",
	"uploads_use_yearmonth_folders", "thumbnail_size_w", "thumbnail_size_h", "thumbnail_crop", "medium_size_w", "medium_size_h",
	"large_size_w", "large_size_h", "site_icon", "WPLANG"}

// wp2pPostOptions are options holding a post id; wp2pTermOptions a term id.
var wp2pPostOptions = map[string]bool{"page_on_front": true, "page_for_posts": true, "wp_page_for_privacy_policy": true, "site_icon": true}
var wp2pTermOptions = map[string]bool{"default_category": true}

// wp2pOptionValue converts a WordPress option value (a string, or an array for serialized options)
// to the type of def, the value Pursae stores by default.
func wp2pOptionValue(v, def any) (any, bool) {
	str := ""
	switch t := v.(type) {
	case string:
		str = t
	case float64:
		str = strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			str = "yes"
		} else {
			str = "no"
		}
	case nil:
	default:
		switch def.(type) {
		case []string, []any, map[string]any:
			return t, true
		}
		return nil, false
	}
	switch def.(type) {
	case bool:
		return str == "yes" || str == "1" || str == "true" || str == "on", true
	case float64, int:
		f, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
		if err != nil {
			return nil, false
		}
		return f, true
	case []string, []any:
		if str == "" {
			return []any{}, true
		}
		return []any{str}, true
	case map[string]any:
		return nil, false
	}
	return str, true
}

// importOptions moves the site settings and the WooCommerce store settings.
func (r *wp2pRun) importOptions(ctx context.Context) error {
	r.loadContentMaps(ctx)
	raw, err := os.ReadFile(filepath.Join(r.dir, "options.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var o wp2pOptions
	if err := json.Unmarshal(raw, &o); err != nil {
		return fmt.Errorf("options.json: %w", err)
	}
	vals := map[string]any{}
	for _, k := range wp2pCoreOptions {
		v, ok := o.Options[k]
		if !ok {
			continue
		}
		key := k
		if wp2pPostOptions[k] || wp2pTermOptions[k] {
			old, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
			m := r.posts
			if wp2pTermOptions[k] {
				m = r.terms
			}
			if old == 0 {
				vals[key] = float64(0)
				continue
			}
			id, ok := m[old]
			if !ok {
				r.rep.note("The setting %s pointed at an item that was not imported; it was left as it is here.", k)
				continue
			}
			vals[key] = float64(id)
			continue
		}
		def := settings.Defaults[key]
		if def == nil {
			def = ""
		}
		cv, ok := wp2pOptionValue(v, def)
		if !ok {
			r.rep.Unmapped["option "+k]++
			continue
		}
		vals[key] = cv
	}
	// WooCommerce settings: woocommerce_<name> is goocommerce_<name> here (goo_options.go), for the
	// settings Pursae Commerce has; page settings are mapped to the imported pages.
	wooUnmapped := 0
	keys := make([]string, 0, len(o.Options))
	for k := range o.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		short, ok := strings.CutPrefix(k, "woocommerce_")
		if !ok {
			continue
		}
		def, known := gooOptDefaults[short]
		if !known || short == "version" || short == "db_version" {
			wooUnmapped++
			continue
		}
		v := o.Options[k]
		if strings.HasSuffix(short, "_page_id") {
			old, _ := strconv.ParseInt(fmt.Sprint(v), 10, 64)
			if id, ok := r.posts[old]; ok && old != 0 {
				vals[gooOptPrefix+short] = float64(id)
			}
			continue
		}
		if def == nil {
			def = ""
		}
		cv, ok := wp2pOptionValue(v, def)
		if !ok {
			wooUnmapped++
			continue
		}
		vals[gooOptPrefix+short] = cv
	}
	if wooUnmapped > 0 {
		r.rep.Unmapped["WooCommerce settings without a Pursae Commerce equivalent"] = wooUnmapped
	}
	if len(vals) > 0 {
		if err := r.s.settings.Set(ctx, vals); err != nil {
			return err
		}
	}
	r.rep.Counts["settings"] = len(vals)
	r.storePages(ctx, vals)
	// Menu locations: a location of the same name gets the menu, if the location is free here.
	if len(o.MenuLocations) > 0 {
		menus, err := r.s.store.Menus(ctx)
		if err == nil {
			bySlug := map[string]*store.Menu{}
			taken := map[string]bool{}
			for _, m := range menus {
				bySlug[m.Slug] = m
				for _, l := range m.Locations {
					taken[l] = true
				}
			}
			locs := map[string]int64{}
			for loc, slug := range o.MenuLocations {
				m := bySlug[wp.SanitizeTitle(slug)]
				if m == nil {
					m = bySlug[slug]
				}
				if m != nil && !taken[loc] {
					locs[loc] = m.ID
				}
			}
			if len(locs) > 0 {
				_ = r.s.store.SetMenuLocations(ctx, locs)
			}
			if len(locs) < len(o.MenuLocations) {
				r.rep.note("Some menus could not be put in a theme location of the same name: assign them in Appearance › Menus.")
			}
		}
	}
	if o.Theme != "" {
		r.rep.note("The WordPress theme “%s” does not move (PHP themes do not run on Pursae): pick a Pursae theme in Appearance › Themes.", o.Theme)
	}
	if n := len(o.Plugins); n > 0 {
		r.rep.note("%d active WordPress plugin(s) do not move (PHP plugins do not run on Pursae): see Plugins for the built-in equivalents.", n)
	}
	return nil
}

// wp2pStorePageMarkers are what WooCommerce's cart, checkout and account pages hold (the classic
// shortcodes), replaced with Pursae Commerce's own. The Cart and Checkout blocks of WooCommerce
// 8.3+ stay: Pursae Commerce renders them (goo_blocks_cart.go) with their settings.
var wp2pStorePageMarkers = map[string][]string{
	"cart":      {"[woocommerce_cart"},
	"checkout":  {"[woocommerce_checkout"},
	"myaccount": {"[woocommerce_my_account", "wp:woocommerce/customer-account"},
}

// storePages gives the imported cart, checkout and account pages Pursae Commerce's own content
// (gooPages), so the store works once Pursae Commerce is active.
func (r *wp2pRun) storePages(ctx context.Context, vals map[string]any) {
	for _, pg := range gooPages {
		markers := wp2pStorePageMarkers[pg.Key]
		id, _ := vals[gooOptPrefix+pg.Key+"_page_id"].(float64)
		if len(markers) == 0 || id == 0 || pg.Content == "" {
			continue
		}
		p, err := r.s.store.GetPost(ctx, int64(id))
		if err != nil {
			continue
		}
		for _, m := range markers {
			if strings.Contains(p.Content, m) {
				p.Content = pg.Content
				if r.s.store.UpdatePost(ctx, p, r.u.ID) == nil {
					short := pg.Content[strings.Index(pg.Content, "[") : strings.Index(pg.Content, "]")+1]
					r.rep.note("The page “%s” now shows Pursae Commerce's %s instead of WooCommerce's block or shortcode.", p.Title, short)
				}
				break
			}
		}
	}
}

// finish cleans up: term counts, the page cache, the extracted files.
func (r *wp2pRun) finish(ctx context.Context) error {
	if home := strings.TrimRight(r.s.settings.String("home"), "/"); r.man != nil && r.man.HomeURL != "" && !strings.EqualFold(home, strings.TrimRight(r.man.HomeURL, "/")) {
		r.rep.note("Links inside the content still point at %s. If the site moves to %s, run: pursae search-replace '%s' '%s'", r.man.HomeURL, home, strings.TrimRight(r.man.HomeURL, "/"), home)
	}
	if (r.rep.Counts["orders"] > 0 || r.rep.Counts["products"] > 0 || r.rep.Counts["coupons"] > 0) && !r.s.gooOn() {
		r.rep.note("Pursae Commerce is not active: activate it in Plugins to open the store.")
	}
	if n := r.rep.Counts["users_password_reset"]; n > 0 {
		r.rep.note("%d user(s) had a password hash Pursae cannot verify: they sign in through “Lost your password?”.", n)
	}
	r.s.purgeAll(ctx)
	_ = os.RemoveAll(r.dir)
	return nil
}

// wp2pLoadRun builds the run of a wp2p_migrations row.
func (s *Server) wp2pLoadRun(ctx context.Context, id string) (*wp2pRun, error) {
	var key []byte
	var createdBy int64
	var src string
	var prog, rep []byte
	if err := s.store.Pool.QueryRow(ctx, `SELECT package_key, created_by, source, progress, report FROM wp2p_migrations WHERE id = $1`, id).
		Scan(&key, &createdBy, &src, &prog, &rep); err != nil {
		return nil, errors.New("this import is not known")
	}
	r := &wp2pRun{s: s, id: id, dir: s.wp2pWorkDir(id), pkg: s.wp2pPackagePath(id), src: src, prog: &wp2pProgress{}, rep: newWP2PReport()}
	if len(key) == 32 {
		k := wp2pKey(key)
		r.key = &k
	}
	if len(prog) > 0 {
		_ = json.Unmarshal(prog, r.prog)
	}
	if len(rep) > 2 {
		_ = json.Unmarshal(rep, r.rep)
		if r.rep.Counts == nil {
			*r.rep = *newWP2PReport()
		}
		if r.rep.Existing == nil {
			r.rep.Existing = map[string]int{}
		}
		if r.rep.Unmapped == nil {
			r.rep.Unmapped = map[string]int{}
		}
	}
	u, err := s.store.UserByID(ctx, createdBy)
	if err != nil {
		u, err = s.wp2pFirstAdmin(ctx)
		if err != nil {
			return nil, errors.New("there is no administrator to import as")
		}
	}
	r.u = u
	return r, nil
}

// wp2pFirstAdmin is the administrator with the lowest id.
func (s *Server) wp2pFirstAdmin(ctx context.Context) (*store.User, error) {
	var id int64
	if err := s.store.Pool.QueryRow(ctx, `SELECT u.id FROM users u JOIN user_roles ur ON ur.user_id = u.id JOIN roles r ON r.id = ur.role_id
		WHERE r.slug = 'administrator' ORDER BY u.id LIMIT 1`).Scan(&id); err != nil {
		return nil, err
	}
	return s.store.UserByID(ctx, id)
}

// wp2pSetState updates the state (and error) of a run.
func (s *Server) wp2pSetState(ctx context.Context, id, state, msg string) {
	_, _ = s.store.Pool.Exec(context.WithoutCancel(ctx), `UPDATE wp2p_migrations SET state = $2, error = $3, updated_at = now() WHERE id = $1`, id, state, msg)
}

// wp2pImportJob is the background run ("wp2p_import" job, payload {"id": ...}). A failure is recorded
// on the row (the screen offers Resume, which queues the job again); a shutdown leaves the job queued.
func (s *Server) wp2pImportJob(ctx context.Context, raw map[string]any) error {
	id, _ := raw["id"].(string)
	if !wxrTokenRE.MatchString(id) {
		return errors.New("invalid WP to Pursae import job")
	}
	var state string
	if s.store.Pool.QueryRow(ctx, `SELECT state FROM wp2p_migrations WHERE id = $1`, id).Scan(&state) != nil {
		return nil //nolint:nilerr // the row was deleted
	}
	if state != "queued" && state != "running" {
		return nil
	}
	r, err := s.wp2pLoadRun(ctx, id)
	if err != nil {
		s.wp2pSetState(ctx, id, "failed", err.Error())
		return nil
	}
	s.wp2pSetState(ctx, id, "running", "")
	if err := r.run(ctx); err != nil {
		if ctx.Err() != nil {
			s.wp2pSetState(ctx, id, "queued", "")
			return err
		}
		s.wp2pSetState(ctx, id, "failed", err.Error())
		return nil
	}
	s.wp2pSetState(ctx, id, "done", "")
	return nil
}

// wp2pNewID is a random import id (32 hex digits).
func wp2pNewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
