package server

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"wpgo/internal/config"
)

// bmgrTableInfo describes one table in an archive.
type bmgrTableInfo struct {
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Rows     int64    `json:"rows"`
	Columns  []string `json:"columns"`
}

// bmgrFilesInfo describes one files group in an archive.
type bmgrFilesInfo struct {
	Group string `json:"group"`
	Count int    `json:"count"`
	Bytes int64  `json:"bytes"`
}

// bmgrManifest is manifest.json.
type bmgrManifest struct {
	Format     int             `json:"format"`
	Created    time.Time       `json:"created"`
	Kind       string          `json:"kind"` // manual | safety | scheduled
	Version    string          `json:"version"`
	Schema     string          `json:"schema"` // newest applied migration
	SiteURL    string          `json:"site_url"`
	SiteName   string          `json:"site_name"`
	Note       string          `json:"note,omitempty"`
	Categories []string        `json:"categories"`
	UserMode   string          `json:"user_mode,omitempty"` // all | roles | ids
	Users      int             `json:"users,omitempty"`
	Tables     []bmgrTableInfo `json:"tables"`
	Files      []bmgrFilesInfo `json:"files"`
	// Checksums holds the SHA-256 (hex) of every archive entry but the manifest, so a damaged or
	// half-copied archive is caught before a restore starts. Older archives have none.
	Checksums map[string]string `json:"checksums,omitempty"`
	// RemoteOnly counts media files that live only in the Pursae CDN bucket ("Remove files from the
	// server"): the archive holds their rows and the CDN index but not the files themselves.
	RemoteOnly int `json:"remote_only,omitempty"`
	// Skipped counts files that could not be read and are therefore NOT in the archive.
	Skipped int `json:"skipped,omitempty"`
	// SiteKey marks an archive that carries the site's encryption key (bmgrSelection.IncludeKey).
	SiteKey bool `json:"site_key,omitempty"`
}

// bmgrSelection is what a backup is asked to contain.
type bmgrSelection struct {
	Categories []string
	UserMode   string // all | roles | ids
	Roles      []string
	UserIDs    []int64
	Note       string
	Kind       string
	NoFiles    bool     // the tables only: no media, theme or plugin files (pre-update backups)
	KeepFiles  []string // with NoFiles: the file groups still written (pre-update backups keep "plugins" and "themes")
	// IncludeKey writes the site's encryption key (auth_secret) into the archive, so its sealed
	// secrets can be read after a restore on another server. Off by default: whoever holds such an
	// archive can decrypt every stored secret and forge Sign() tokens.
	IncludeKey bool
}

// bmgrEntry is a stored archive as the screen lists it.
type bmgrEntry struct {
	Name     string
	Size     int64
	Modified time.Time
	M        *bmgrManifest
	Err      string
}

var bmgrNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}\.zip$`)

// bmgrCompressed are the file types stored as they are: deflating them again only costs time.
var bmgrCompressed = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".avif": true, ".heic": true,
	".mp4": true, ".webm": true, ".mov": true, ".mp3": true, ".m4a": true, ".ogg": true, ".zip": true, ".gz": true, ".br": true,
	".7z": true, ".rar": true, ".pdf": true, ".woff": true, ".woff2": true, ".wasm": false}

func bmgrToken() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------- job bookkeeping

func (s *Server) bmgrStart(kind string) (*bmgrJob, error) {
	s.bmgr.mu.Lock()
	defer s.bmgr.mu.Unlock()
	if s.bmgr.busy {
		return nil, errors.New("another backup or restore is still running")
	}
	if s.bmgr.jobs == nil {
		s.bmgr.jobs = map[string]*bmgrJob{}
	}
	s.bmgrPruneJobsLocked()
	j := &bmgrJob{ID: bmgrToken() + bmgrToken(), Kind: kind, State: "running", Phase: "Starting", Started: time.Now()}
	s.bmgr.jobs[j.ID] = j
	s.bmgr.busy = true
	return j, nil
}

// bmgrKeepJobs is how many finished jobs stay in memory for the screen to show; older ones are
// dropped so scheduled backups do not grow the job list (and its logs) for the life of the process.
// The archives themselves are untouched.
const bmgrKeepJobs = 20

// bmgrPruneJobsLocked drops all but the bmgrKeepJobs most recently finished jobs. Running jobs are
// always kept. The caller holds s.bmgr.mu.
func (s *Server) bmgrPruneJobsLocked() {
	var done []*bmgrJob
	for _, j := range s.bmgr.jobs {
		if j.State != "running" {
			done = append(done, j)
		}
	}
	if len(done) <= bmgrKeepJobs {
		return
	}
	sort.Slice(done, func(a, b int) bool { return done[a].Finished.After(done[b].Finished) })
	for _, j := range done[bmgrKeepJobs:] {
		delete(s.bmgr.jobs, j.ID)
	}
}

func (s *Server) bmgrUpdate(j *bmgrJob, f func(*bmgrJob)) {
	s.bmgr.mu.Lock()
	defer s.bmgr.mu.Unlock()
	f(j)
}

func (s *Server) bmgrLog(j *bmgrJob, format string, a ...any) {
	s.bmgrUpdate(j, func(j *bmgrJob) {
		j.Log = append(j.Log, time.Now().Format("15:04:05")+"  "+fmt.Sprintf(format, a...))
		if len(j.Log) > 300 {
			j.Log = j.Log[len(j.Log)-300:]
		}
	})
}

func (s *Server) bmgrProgress(j *bmgrJob, phase string, pct int) {
	s.bmgrUpdate(j, func(j *bmgrJob) { j.Phase, j.Percent = phase, min(max(pct, 0), 100) })
}

func (s *Server) bmgrFinish(j *bmgrJob, err error) {
	s.bmgr.mu.Lock()
	defer s.bmgr.mu.Unlock()
	j.Finished = time.Now()
	if cancel := s.bmgr.cancels[j.ID]; cancel != nil {
		cancel()
		delete(s.bmgr.cancels, j.ID)
	}
	switch {
	case errors.Is(err, context.Canceled):
		j.State, j.Error, j.Phase = "error", "Canceled by an administrator", "Canceled"
	case err != nil:
		j.State, j.Error, j.Phase = "error", err.Error(), "Failed"
	case j.Warnings > 0:
		j.State, j.Phase, j.Percent = "done", "Done with warnings", 100
	default:
		j.State, j.Phase, j.Percent = "done", "Done", 100
	}
	s.bmgr.busy = false
}

// bmgrCancelable returns a context for a running backup job that bmgrCancel can stop.
func (s *Server) bmgrCancelable(j *bmgrJob) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	s.bmgr.mu.Lock()
	defer s.bmgr.mu.Unlock()
	if s.bmgr.cancels == nil {
		s.bmgr.cancels = map[string]context.CancelFunc{}
	}
	s.bmgr.cancels[j.ID] = cancel
	return ctx
}

// bmgrCancel stops a running backup; false when the job is unknown, finished or not cancelable.
func (s *Server) bmgrCancel(id string) bool {
	s.bmgr.mu.Lock()
	defer s.bmgr.mu.Unlock()
	cancel := s.bmgr.cancels[id]
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// bmgrPartMaxAge is how old a leftover ".part" file must be before it is swept: a crashed backup or
// upload leaves one behind and nothing else ever removes it.
const bmgrPartMaxAge = time.Hour

// bmgrSweepParts deletes stale half-written archives and returns how many it removed.
func (s *Server) bmgrSweepParts() int {
	ents, _ := os.ReadDir(s.bmgrDir())
	n := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip.part") {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > bmgrPartMaxAge {
			if os.Remove(filepath.Join(s.bmgrDir(), e.Name())) == nil {
				n++
			}
		}
	}
	return n
}

// bmgrSafetyKeep is how many pre-restore snapshots are kept.
const bmgrSafetyKeep = 3

// bmgrUsage is the total size of the archives in the backups directory.
func (s *Server) bmgrUsage() int64 {
	var n int64
	for _, e := range s.bmgrList() {
		n += e.Size
	}
	return n
}

func (s *Server) bmgrJobByID(id string) *bmgrJob {
	s.bmgr.mu.Lock()
	defer s.bmgr.mu.Unlock()
	j := s.bmgr.jobs[id]
	if j == nil {
		return nil
	}
	cp := *j
	cp.Log = append([]string(nil), j.Log...)
	return &cp
}

// ---------------------------------------------------------------- backup

// bmgrResolve turns a selection into the table set and the files groups it carries.
func (sc *bmgrSchema) resolve(sel bmgrSelection) (set map[string]bool, groups []string) {
	chosen := map[string]bool{}
	for _, k := range sel.Categories {
		chosen[k] = true
	}
	base := map[string]bool{}
	for _, t := range sc.Tables {
		if chosen[bmgrCategoryOf(t)] {
			base[t] = true
		}
	}
	set = sc.closure(base)
	for _, c := range bmgrCategories {
		if chosen[c.Key] && c.Files != "" {
			groups = append(groups, c.Files)
		}
	}
	return set, groups
}

// bmgrRunBackup writes a backup archive and returns its file name.
func (s *Server) bmgrRunBackup(ctx context.Context, j *bmgrJob, sel bmgrSelection) (string, error) {
	dir := s.bmgrDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sc, err := bmgrLoadSchema(ctx, s.store.Pool)
	if err != nil {
		return "", err
	}
	set, groups := sc.resolve(sel)
	if sel.NoFiles {
		var keep []string
		for _, g := range groups {
			if contains(sel.KeepFiles, g) {
				keep = append(keep, g)
			}
		}
		groups = keep
	}
	if len(set) == 0 && len(groups) == 0 {
		return "", errors.New("nothing is selected")
	}
	if err := s.bmgrCheckSpace(ctx, dir, sc, set, groups); err != nil {
		return "", err
	}
	kind := sel.Kind
	if kind == "" {
		kind = "manual"
	}
	prefix := "backup"
	switch kind {
	case "safety":
		prefix = "pre-restore"
	case bmgrScheduleKind:
		prefix = bmgrScheduleKind
	case bmgrPreUpdateKind:
		prefix = bmgrPreUpdateKind
	}
	name := fmt.Sprintf("%s-%s-%s.zip", prefix, time.Now().Format("20060102-150405"), bmgrToken())
	tmp := filepath.Join(dir, name+".part")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(tmp)
		}
	}()
	zw := zip.NewWriter(f)
	sums := map[string]string{}

	var ids []int64
	if set["users"] {
		ids, err = s.bmgrUserIDs(ctx, sel)
		if err != nil {
			return "", err
		}
	}
	filters := map[string]string{}
	if sel.UserMode == "roles" || sel.UserMode == "ids" {
		filters = sc.userFilters(set)
	}

	m := bmgrManifest{Format: bmgrFormat, Created: time.Now().UTC(), Kind: kind, Version: Version, SiteURL: bmgrSiteURL(s), SiteName: s.settings.String("blogname"),
		Note: sel.Note, Categories: sel.Categories, UserMode: sel.UserMode, SiteKey: sel.IncludeKey && set["settings"]}
	if m.SiteKey {
		s.bmgrLog(j, "this archive includes the site's encryption key: keep it private, anyone who has it can read the stored passwords and API keys")
		if s.cfg.Secret != "" {
			s.bmgrLog(j, "note: this site encrypts with PURSAE_SECRET, which is not stored in archives; set the same PURSAE_SECRET on the new server")
		}
	}
	if len(filters) > 0 {
		m.Users = len(ids)
	}
	_ = s.store.Pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), '') FROM schema_migrations`).Scan(&m.Schema)

	// The whole export sees one snapshot of the database.
	tx, err := s.store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var tables []string
	for t := range set {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for i, t := range tables {
		s.bmgrProgress(j, "Database: "+t, i*40/max(len(tables), 1))
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "database/" + t + ".jsonl", Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return "", err
		}
		hw := sha256.New()
		n, err := bmgrExportTable(ctx, tx, t, filters[t], ids, bmgrExcludedSettings(sel.IncludeKey), io.MultiWriter(w, hw))
		sums["database/"+t+".jsonl"] = hex.EncodeToString(hw.Sum(nil))
		if err != nil {
			return "", fmt.Errorf("exporting %s: %w", t, err)
		}
		cols := make([]string, 0, len(sc.Columns[t]))
		for _, c := range sc.Columns[t] {
			cols = append(cols, c.Name)
		}
		m.Tables = append(m.Tables, bmgrTableInfo{Name: t, Category: bmgrCategoryOf(t), Rows: n, Columns: cols})
		s.bmgrLog(j, "%s: %d rows", t, n)
	}
	_ = tx.Rollback(ctx)

	if contains(groups, "uploads") {
		var n int
		if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM gocdn_items WHERE local_removed`).Scan(&n); err == nil && n > 0 {
			m.RemoteOnly = n
			s.bmgrLog(j, "WARNING: %d media files exist only in the Pursae CDN bucket (\"Remove files from the server\"). This archive holds their database rows and the CDN index but NOT the files: keep the bucket, or copy them back to the server (Pursae CDN > Bulk) before relying on this backup.", n)
		}
	}

	roots := s.bmgrFileRoots()
	skip := func(g, rel string, err error) {
		m.Skipped++
		s.bmgrUpdate(j, func(j *bmgrJob) { j.Warnings++ })
		if m.Skipped <= 50 {
			s.bmgrLog(j, "WARNING: skipped %s/%s, it could not be read: %v", g, filepath.ToSlash(rel), err)
		}
	}
	for gi, g := range groups {
		root := roots[g]
		info := bmgrFilesInfo{Group: g}
		var total int64
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if fi, e := d.Info(); e == nil {
					total += fi.Size()
				}
			}
			return nil
		})
		var done int64
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() || strings.HasSuffix(p, ".part") {
				return nil // directories, symlinks and half-written files are not backed up
			}
			rel, _ := filepath.Rel(root, p)
			fi, err := d.Info()
			if err != nil {
				skip(g, rel, err)
				return nil
			}
			src, err := os.Open(p)
			if err != nil {
				skip(g, rel, err)
				return nil
			}
			method := zip.Deflate
			if bmgrCompressed[strings.ToLower(filepath.Ext(p))] {
				method = zip.Store
			}
			hdr, _ := zip.FileInfoHeader(fi)
			hdr.Name, hdr.Method = "files/"+g+"/"+filepath.ToSlash(rel), method
			w, err := zw.CreateHeader(hdr)
			if err != nil {
				src.Close()
				return err
			}
			hw := sha256.New()
			_, err = io.Copy(io.MultiWriter(w, hw), src)
			src.Close()
			sums[hdr.Name] = hex.EncodeToString(hw.Sum(nil))
			if err != nil {
				return err
			}
			info.Count++
			info.Bytes += fi.Size()
			done += fi.Size()
			if info.Count%25 == 0 {
				s.bmgrProgress(j, "Files: "+g, 40+int((int64(gi)*100+done*100/max(total, 1))*55/int64(max(len(groups), 1)*100)))
			}
			return ctx.Err()
		})
		if err != nil {
			return "", fmt.Errorf("adding %s files: %w", g, err)
		}
		m.Files = append(m.Files, info)
		s.bmgrLog(j, "%s files: %d (%s)", g, info.Count, bmgrHuman(info.Bytes))
	}

	if m.Skipped > 0 {
		s.bmgrLog(j, "WARNING: %d files could not be read and are NOT in this archive", m.Skipped)
	}
	s.bmgrProgress(j, "Finishing", 98)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate, Modified: time.Now()})
	if err != nil {
		return "", err
	}
	m.Checksums = sums
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return "", err
	}
	ok = true
	s.bmgrRemoteAfterBackup(ctx, j, name, sel.Kind)
	return name, nil
}

// bmgrUserIDs are the users a backup keeps.
func (s *Server) bmgrUserIDs(ctx context.Context, sel bmgrSelection) ([]int64, error) {
	var q string
	var args []any
	switch sel.UserMode {
	case "roles":
		q = `SELECT DISTINCT ur.user_id FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE r.slug = ANY($1)`
		args = []any{sel.Roles}
	case "ids":
		q = `SELECT id FROM users WHERE id = ANY($1)`
		args = []any{sel.UserIDs}
	default:
		q = `SELECT id FROM users`
	}
	rows, err := s.store.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func bmgrSiteURL(s *Server) string {
	u := strings.TrimRight(s.settings.String("home"), "/")
	if u == "" {
		u = s.cfg.SiteURL
	}
	return u
}

// ---------------------------------------------------------------- listing and reading

// bmgrReadManifest reads manifest.json of an archive file.
func bmgrReadManifest(file string) (*bmgrManifest, error) {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return nil, errors.New("not a valid backup archive")
	}
	defer zr.Close()
	return bmgrManifestOf(&zr.Reader)
}

func bmgrManifestOf(zr *zip.Reader) (*bmgrManifest, error) {
	for _, f := range zr.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var m bmgrManifest
		if err := json.NewDecoder(io.LimitReader(rc, 32<<20)).Decode(&m); err != nil {
			return nil, errors.New("the manifest is damaged")
		}
		if m.Format < 1 || m.Format > bmgrFormat {
			return nil, fmt.Errorf("the archive format %d is not supported by this version", m.Format)
		}
		return &m, nil
	}
	return nil, errors.New("manifest.json is missing: this is not a Pursae backup")
}

// bmgrList is every archive in the backups directory, newest first.
func (s *Server) bmgrList() []bmgrEntry {
	s.bmgrSweepParts()
	ents, _ := os.ReadDir(s.bmgrDir())
	var out []bmgrEntry
	for _, e := range ents {
		if e.IsDir() || !bmgrNameRe.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		be := bmgrEntry{Name: e.Name(), Size: fi.Size(), Modified: fi.ModTime()}
		if m, err := bmgrReadManifest(filepath.Join(s.bmgrDir(), e.Name())); err != nil {
			be.Err = err.Error()
		} else {
			be.M = m
		}
		out = append(out, be)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// bmgrPath resolves a stored archive name; "" when the name is not one of ours.
func (s *Server) bmgrPath(name string) string {
	if !bmgrNameRe.MatchString(name) || name != path.Base(name) {
		return ""
	}
	p := filepath.Join(s.bmgrDir(), name)
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}

// ---------------------------------------------------------------- restore

// bmgrRestoreReq is what a restore is asked to do.
type bmgrRestoreReq struct {
	File       string
	Categories []string
	RewriteURL bool
	Safety     bool
	Me         int64
}

// bmgrPlan is the table set and files groups a restore will touch, and what it warns about.
type bmgrPlan struct {
	Tables map[string]bool
	Groups []string
	Notes  []string
}

// bmgrMakePlan checks the archive against the live schema and works out what a restore replaces.
func (s *Server) bmgrMakePlan(ctx context.Context, m *bmgrManifest, cats []string) (*bmgrPlan, *bmgrSchema, error) {
	sc, err := bmgrLoadSchema(ctx, s.store.Pool)
	if err != nil {
		return nil, nil, err
	}
	var live string
	_ = s.store.Pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), '') FROM schema_migrations`).Scan(&live)
	if m.Schema > live {
		return nil, nil, fmt.Errorf("the backup comes from a newer Pursae (schema %s, this site has %s): update this site first", m.Schema, live)
	}
	chosen := map[string]bool{}
	for _, k := range cats {
		if bmgrCategoryByKey(k) != nil {
			chosen[k] = true
		}
	}
	inArchive := map[string]bool{}
	for _, t := range m.Tables {
		inArchive[t.Name] = true
	}
	plan := &bmgrPlan{Tables: map[string]bool{}}
	for _, t := range m.Tables {
		if chosen[t.Category] && sc.has(t.Name) {
			plan.Tables[t.Name] = true
		} else if chosen[t.Category] {
			plan.Notes = append(plan.Notes, "table "+t.Name+" does not exist on this site and is skipped")
		}
	}
	closed := sc.closure(plan.Tables)
	for t := range closed {
		if plan.Tables[t] {
			continue
		}
		if inArchive[t] {
			plan.Tables[t] = true
			plan.Notes = append(plan.Notes, t+" is restored too: it depends on a table you selected")
		} else if t == "menu_locations" && bmgrLegacyMenuLocations(m) {
			plan.Tables[t] = true // filled from the old menus.location column (bmgrRestoreLegacyMenuLocations)
			plan.Notes = append(plan.Notes, "menu locations are taken from the menus of this older backup")
		} else {
			plan.Tables[t] = true // emptied by the cascade anyway; nothing to put back
			plan.Notes = append(plan.Notes, t+" is not in the backup and is emptied: it depends on a table you selected")
		}
	}
	have := map[string]bool{}
	for _, f := range m.Files {
		have[f.Group] = true
	}
	for _, k := range cats {
		if c := bmgrCategoryByKey(k); c != nil && c.Files != "" && have[c.Files] {
			plan.Groups = append(plan.Groups, c.Files)
		}
	}
	return plan, sc, nil
}

// bmgrLegacyMenuLocations reports whether the archive predates menu_locations and keeps each menu's
// location in a menus.location column (0.1.7 and older).
func bmgrLegacyMenuLocations(m *bmgrManifest) bool {
	legacy := false
	for _, t := range m.Tables {
		switch t.Name {
		case "menu_locations":
			return false
		case "menus":
			legacy = contains(t.Columns, "location")
		}
	}
	return legacy
}

// bmgrRunRestore restores the chosen parts of an archive.
func (s *Server) bmgrRunRestore(ctx context.Context, j *bmgrJob, req bmgrRestoreReq) error {
	file := s.bmgrPath(req.File)
	if file == "" {
		return errors.New("the backup file was not found")
	}
	zr, err := zip.OpenReader(file)
	if err != nil {
		return errors.New("not a valid backup archive")
	}
	defer zr.Close()
	m, err := bmgrManifestOf(&zr.Reader)
	if err != nil {
		return err
	}
	plan, sc, err := s.bmgrMakePlan(ctx, m, req.Categories)
	if err != nil {
		return err
	}
	if len(plan.Tables) == 0 && len(plan.Groups) == 0 {
		return errors.New("nothing to restore: select at least one part")
	}
	for _, n := range plan.Notes {
		s.bmgrLog(j, "note: %s", n)
	}

	s.bmgrProgress(j, "Verifying the archive", 1)
	budget := bmgrNewUnpackBudget(&zr.Reader)
	if err := budget.precheck(&zr.Reader); err != nil {
		return fmt.Errorf("the archive was refused, nothing was restored: %w", err)
	}
	if err := bmgrVerifyArchive(ctx, &zr.Reader, m); err != nil {
		return fmt.Errorf("the archive failed verification, nothing was restored: %w", err)
	}
	s.bmgrLog(j, "archive verified")
	if m.RemoteOnly > 0 {
		s.bmgrLog(j, "note: %d media files of the source site were only in its Pursae CDN bucket and are not in this archive", m.RemoteOnly)
	}

	if req.Safety {
		s.bmgrProgress(j, "Safety snapshot of this site", 1)
		var cats []string
		for _, c := range bmgrCategories {
			cats = append(cats, c.Key)
		}
		name, err := s.bmgrRunBackup(ctx, j, bmgrSelection{Categories: cats, UserMode: "all", Kind: "safety", Note: "Taken automatically before restoring " + req.File})
		if err != nil {
			return fmt.Errorf("the safety snapshot failed, nothing was restored: %w", err)
		}
		s.bmgrLog(j, "safety snapshot saved as %s", name)
		if removed := s.bmgrPruneKind("safety", "pre-restore-", bmgrSafetyKeep); len(removed) > 0 {
			s.bmgrLog(j, "removed %d older safety snapshots (the newest %d are kept)", len(removed), bmgrSafetyKeep)
		}
	}

	byName := map[string]*zip.File{}
	for _, f := range zr.File {
		byName[f.Name] = f
	}
	cols := map[string][]string{}
	for _, t := range m.Tables {
		cols[t.Name] = t.Columns
	}
	oldURL, newURL := "", ""
	if req.RewriteURL {
		oldURL, newURL = strings.TrimRight(m.SiteURL, "/"), strings.TrimRight(bmgrSiteURL(s), "/")
		if oldURL != "" && oldURL != newURL {
			s.bmgrLog(j, "rewriting %s to %s", oldURL, newURL)
		}
	}

	// Files are unpacked into a staging folder first: a full disk, a bad permission or an unsafe path
	// stops the restore before the database is touched. They are put in place with renames just
	// before the database commits, and those renames are undone if it does not (backup_swap.go).
	roots := s.bmgrFileRoots()
	staged := map[string]string{} // group -> staging folder
	defer func() {
		for _, d := range staged {
			os.RemoveAll(d)
		}
	}()
	for gi, g := range plan.Groups {
		root := roots[g]
		if root == "" {
			continue
		}
		resolved, stage, err := bmgrStageDir(root, g)
		if err != nil {
			return fmt.Errorf("preparing the %s files, nothing was restored: %w", g, err)
		}
		roots[g], staged[g] = resolved, stage
		prefix := "files/" + g + "/"
		var n int
		for _, f := range zr.File {
			rel, ok := strings.CutPrefix(f.Name, prefix)
			if !ok || f.FileInfo().IsDir() {
				continue
			}
			dst, err := bmgrSafeJoin(stage, rel)
			if err != nil {
				return fmt.Errorf("unsafe path in the archive, nothing was restored: %s", f.Name)
			}
			if err := bmgrExtractLimited(f, dst, budget); err != nil {
				return fmt.Errorf("unpacking %s, nothing was restored: %w", f.Name, err)
			}
			n++
			if n%25 == 0 {
				s.bmgrProgress(j, "Unpacking files: "+g, 2+gi*3/max(len(plan.Groups), 1))
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}

	// What this site granted its plugins, before the archive's plugins table replaces it (backup_plugins.go).
	restoresPlugins := plan.Tables["plugins"] || contains(plan.Groups, "plugins")
	var pluginsBefore map[string]bmgrPluginGrant
	if restoresPlugins {
		pluginsBefore = s.bmgrPluginGrants(ctx)
	}

	// Database: one transaction, all or nothing; the files are swapped in right before it commits.
	var swap *bmgrSwap
	if len(plan.Tables) == 0 {
		if swap, err = s.bmgrSwapIn(ctx, j, plan.Groups, staged, roots); err != nil {
			return fmt.Errorf("nothing was restored: %w", err)
		}
	} else {
		// Secrets sealed under the archive's site secret are re-encrypted for this site (backup_secrets.go).
		var reseal *bmgrResealer
		if to := s.siteSecret(); to != "" {
			reseal = &bmgrResealer{from: bmgrArchiveSecret(byName["database/settings.jsonl"]), to: to}
		}
		conn, err := s.store.Pool.Acquire(ctx)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			conn.Release()
			return err
		}
		var open []io.Closer
		err = bmgrRestoreDB(ctx, tx, sc, bmgrRestoreOpts{
			Tables: plan.Tables, Columns: cols, OldURL: oldURL, NewURL: newURL, Me: req.Me,
			Open: func(t string) (io.Reader, error) {
				f := byName["database/"+t+".jsonl"]
				if f == nil {
					return nil, nil
				}
				rc, err := f.Open()
				if err == nil {
					open = append(open, rc)
				}
				return rc, err
			},
			Progress:  func(i, n int, t string) { s.bmgrProgress(j, "Database: "+t, 5+i*55/max(n, 1)) },
			LogWriter: func(msg string) { s.bmgrLog(j, "%s", msg) },
			Reseal:    reseal,
		})
		for _, c := range open {
			c.Close()
		}
		if err == nil && plan.Tables["plugins"] {
			var n int
			if n, err = bmgrRestrictRestoredGrants(ctx, tx, pluginsBefore); n > 0 {
				s.bmgrLog(j, "%d restored plugins had content:html or admin:scripts granted on the other site: grant them again on the Plugins screen", n)
			}
		}
		if err == nil {
			if swap, err = s.bmgrSwapIn(ctx, j, plan.Groups, staged, roots); err != nil {
				err = fmt.Errorf("nothing was restored: %w", err)
			}
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			conn.Release()
			if rerr := swap.rollback(); rerr != nil {
				return fmt.Errorf("saving the database failed (%v) and putting the previous files back failed too: %v", err, rerr)
			}
			return fmt.Errorf("saving the database failed, nothing was restored: %w", err)
		}
		conn.Release()
		s.bmgrLog(j, "database restored: %d tables", len(plan.Tables))
		if reseal != nil && reseal.n > 0 {
			s.bmgrLog(j, "%d stored secrets (mail, CDN, payment, API and translation keys, two-factor apps) were re-encrypted for this site; Pursae Security backup codes made on the other site must be generated again", reseal.n)
		}
		if reseal != nil && reseal.unreadable > 0 {
			s.bmgrUpdate(j, func(j *bmgrJob) { j.Warnings++; j.Unreadable = reseal.unreadable })
			s.bmgrLog(j, "WARNING: %d stored secrets (mail, CDN, payment, API and translation keys, two-factor apps) cannot be decrypted on this site: the archive was made without the site's encryption key. They read as empty: enter them again, and users with an authenticator app must set it up again", reseal.unreadable)
		}
		if err := s.settings.Load(ctx); err != nil {
			slog.Warn("backup: reloading settings", "err", err)
		}
		s.sealStoredSecrets(ctx) // an archive from an older version may hold plain secrets
		s.purgeAll(ctx)
	}

	for _, g := range plan.Groups {
		if n, ok := swap.placed[g]; ok {
			s.bmgrLog(j, "%s files restored: %d", g, n)
		}
	}
	moved, where, qerr := swap.finish(s.bmgrQuarantineDir(), time.Now())
	if moved > 0 {
		s.bmgrUpdate(j, func(j *bmgrJob) { j.Warnings++; j.Quarantined, j.QuarantineDir = moved, where })
		s.bmgrLog(j, "%d plugin or theme files that are not in the archive were moved to %s (not deleted): check them, then delete the folder", moved, where)
	}
	if qerr != nil {
		s.bmgrLog(j, "WARNING: moving files that are not in the archive aside: %v", qerr)
	}
	if restoresPlugins {
		s.bmgrVetRestoredPlugins(ctx, j, pluginsBefore)
		s.bmgrLog(j, "restart Pursae to load the restored plugins")
	}
	return nil
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// bmgrPlaceWithin returns the path at which dst should be created so the file stays inside root.
// A symlink used as a directory is followed only when its target is still inside root; one that
// points outside is refused before any directory is created through it. The returned path has no
// symlink as a parent, so the following rename does not write through one.
func bmgrPlaceWithin(root, dst string) (string, error) {
	rel, err := filepath.Rel(root, dst)
	if err != nil || !filepath.IsLocal(rel) {
		return "", errors.New("invalid path")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	cur := root
	if parent := filepath.Dir(rel); parent != "." {
		for _, elem := range strings.Split(parent, string(filepath.Separator)) {
			if elem == "" || elem == "." || elem == ".." {
				return "", errors.New("invalid path")
			}
			next := filepath.Join(cur, elem)
			fi, lerr := os.Lstat(next)
			switch {
			case os.IsNotExist(lerr):
				if err := os.Mkdir(next, 0o755); err != nil {
					return "", err
				}
				cur = next
			case lerr != nil:
				return "", lerr
			case fi.Mode()&os.ModeSymlink != 0:
				resolved, err := filepath.EvalSymlinks(next)
				if err != nil {
					return "", err
				}
				if err := bmgrInside(resolvedRoot, resolved); err != nil {
					return "", err
				}
				info, err := os.Stat(resolved)
				if err != nil {
					return "", err
				}
				if !info.IsDir() {
					return "", fmt.Errorf("%s is not a directory", elem)
				}
				cur = resolved
			case !fi.IsDir():
				return "", fmt.Errorf("%s is not a directory", elem)
			default:
				cur = next
			}
		}
	}
	resolvedCur, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", err
	}
	if err := bmgrInside(resolvedRoot, resolvedCur); err != nil {
		return "", err
	}
	return filepath.Join(resolvedCur, filepath.Base(dst)), nil
}

// bmgrInside reports whether path is root or a path inside it.
func bmgrInside(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		return errors.New("refusing to follow a symlink out of the restore directory")
	}
	return nil
}

// bmgrSafeJoin joins an archive path under root, refusing anything that would leave it.
func bmgrSafeJoin(root, rel string) (string, error) {
	if rel == "" || !fs.ValidPath(rel) || strings.ContainsRune(rel, '\\') {
		return "", errors.New("invalid path")
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// bmgrDefaultMaxUnpacked is the default of PURSAE_BACKUP_MAX_UNPACKED.
const bmgrDefaultMaxUnpacked int64 = 20 << 30

// bmgrUnpackRatio: an archive may always unpack to this many times its compressed size, so a
// large real backup is not refused by the fixed cap.
const bmgrUnpackRatio = 20

// errBmgrTooLarge is a restore refused by the unpacked-size limit (a zip bomb or a mislabeled entry).
var errBmgrTooLarge = errors.New("the archive unpacks to more than the allowed size (PURSAE_BACKUP_MAX_UNPACKED)")

// bmgrMaxUnpacked is PURSAE_BACKUP_MAX_UNPACKED in bytes (plain number, or with a K/M/G/T suffix).
func bmgrMaxUnpacked() int64 {
	v := strings.ToUpper(strings.TrimSpace(config.Env("BACKUP_MAX_UNPACKED")))
	v = strings.TrimSuffix(strings.TrimSuffix(v, "B"), "I")
	mult := int64(1)
	if v != "" {
		switch v[len(v)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult > 1 {
			v = v[:len(v)-1]
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 || n > math.MaxInt64/mult {
		return bmgrDefaultMaxUnpacked
	}
	return n * mult
}

// bmgrUnpackBudget caps what a restore may write: the total of all unpacked entries is at most
// max(bmgrUnpackRatio × the archive's compressed size, PURSAE_BACKUP_MAX_UNPACKED). The declared
// sizes are checked up front, and the bytes actually written are counted, so an entry whose
// header lies is cut off too.
type bmgrUnpackBudget struct{ limit, used int64 }

func bmgrNewUnpackBudget(zr *zip.Reader) *bmgrUnpackBudget {
	var packed int64
	for _, f := range zr.File {
		packed += int64(min(f.CompressedSize64, uint64(math.MaxInt64/2)))
	}
	limit := bmgrMaxUnpacked()
	if packed < math.MaxInt64/bmgrUnpackRatio && packed*bmgrUnpackRatio > limit {
		limit = packed * bmgrUnpackRatio
	}
	return &bmgrUnpackBudget{limit: limit}
}

// precheck refuses an archive whose declared unpacked sizes already exceed the limit.
func (b *bmgrUnpackBudget) precheck(zr *zip.Reader) error {
	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if f.UncompressedSize64 > uint64(b.limit) || total > uint64(b.limit) {
			return errBmgrTooLarge
		}
	}
	return nil
}

// bmgrExtractLimited is bmgrExtractN charging the written bytes to budget.
func bmgrExtractLimited(f *zip.File, dst string, b *bmgrUnpackBudget) error {
	if f.UncompressedSize64 > uint64(b.limit-b.used) {
		return errBmgrTooLarge
	}
	return bmgrExtractN(f, dst, &b.used, b.limit)
}

// bmgrExtractN writes one archive file atomically (temp file, then rename); *used counts the
// written bytes against limit.
func bmgrExtractN(f *zip.File, dst string, used *int64, limit int64) error {
	if f.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(rc, limit-*used+1))
	*used += n
	if err == nil && *used > limit {
		err = errBmgrTooLarge
	}
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chtimes(tmp, f.Modified, f.Modified)
	return os.Rename(tmp, dst)
}

func bmgrHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// bmgrVerifyArchive reads every entry of an archive and compares it with the manifest's SHA-256
// (archives without checksums are still read through, which checks the zip CRCs): a damaged or
// half-copied archive is refused before anything on the site changes.
func bmgrVerifyArchive(ctx context.Context, zr *zip.Reader, m *bmgrManifest) error {
	seen := map[string]bool{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || f.Name == "manifest.json" {
			continue
		}
		seen[f.Name] = true
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("%s cannot be read: %w", f.Name, err)
		}
		h := sha256.New()
		// Read at most the declared size plus one byte: archive/zip refuses an entry longer than
		// its header says, and precheck has bounded the declared sizes.
		_, err = io.Copy(h, io.LimitReader(rc, int64(min(f.UncompressedSize64, uint64(math.MaxInt64-1)))+1))
		rc.Close()
		if err != nil {
			return fmt.Errorf("%s is damaged: %w", f.Name, err)
		}
		if want, ok := m.Checksums[f.Name]; ok && want != hex.EncodeToString(h.Sum(nil)) {
			return fmt.Errorf("%s does not match its checksum", f.Name)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	for name := range m.Checksums {
		if !seen[name] {
			return fmt.Errorf("%s is listed in the manifest but missing from the archive", name)
		}
	}
	return nil
}
