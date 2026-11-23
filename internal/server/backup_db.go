package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The database half of Backup Manager: schema discovery, the cascade closure of a table set, the
// row export (JSON lines) and the one-transaction restore.

// bmgrColumn is a table column the export carries.
type bmgrColumn struct {
	Name string
	Gen  bool // generated or identity ALWAYS: never written
}

// bmgrFK is a single-column foreign key (composite ones are deferred but not repaired).
type bmgrFK struct {
	Name, Child, Col, Parent, ParentCol string
	Rule                                byte // a no action, r restrict, c cascade, n set null, d set default
	Nullable                            bool
	Composite                           bool
}

// bmgrSchema is the live schema of the public tables.
type bmgrSchema struct {
	Tables  []string
	Columns map[string][]bmgrColumn
	FKs     []bmgrFK
	Serials map[string][]string // table -> columns fed by a sequence
}

func (sc *bmgrSchema) has(t string) bool { _, ok := sc.Columns[t]; return ok }

// bmgrLoadSchema reads tables, columns, foreign keys and sequence columns.
func bmgrLoadSchema(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (*bmgrSchema, error) {
	sc := &bmgrSchema{Columns: map[string][]bmgrColumn{}, Serials: map[string][]string{}}
	rows, err := q.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		if !bmgrNever[t] {
			sc.Tables = append(sc.Tables, t)
			sc.Columns[t] = nil
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	rows, err = q.Query(ctx, `SELECT table_name, column_name, is_generated, COALESCE(identity_generation, ''),
		COALESCE(column_default, ''), is_identity FROM information_schema.columns WHERE table_schema = 'public'
		ORDER BY table_name, ordinal_position`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t, col, gen, idgen, def, isid string
		if err := rows.Scan(&t, &col, &gen, &idgen, &def, &isid); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := sc.Columns[t]; !ok {
			continue
		}
		sc.Columns[t] = append(sc.Columns[t], bmgrColumn{Name: col, Gen: gen == "ALWAYS" || idgen == "ALWAYS"})
		if strings.HasPrefix(def, "nextval(") || isid == "YES" {
			sc.Serials[t] = append(sc.Serials[t], col)
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	rows, err = q.Query(ctx, `SELECT con.conname, cl.relname, a.attname, pcl.relname, pa.attname, con.confdeltype::text,
			NOT a.attnotnull, array_length(con.conkey, 1) > 1
		FROM pg_constraint con
		JOIN pg_class cl ON cl.oid = con.conrelid JOIN pg_namespace n ON n.oid = cl.relnamespace
		JOIN pg_class pcl ON pcl.oid = con.confrelid
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = con.conkey[1]
		JOIN pg_attribute pa ON pa.attrelid = con.confrelid AND pa.attnum = con.confkey[1]
		WHERE con.contype = 'f' AND n.nspname = 'public' ORDER BY 2, 1`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fk bmgrFK
		var rule string
		if err := rows.Scan(&fk.Name, &fk.Child, &fk.Col, &fk.Parent, &fk.ParentCol, &rule, &fk.Nullable, &fk.Composite); err != nil {
			rows.Close()
			return nil, err
		}
		if rule != "" {
			fk.Rule = rule[0]
		}
		sc.FKs = append(sc.FKs, fk)
	}
	rows.Close()
	return sc, rows.Err()
}

// bmgrClosure adds every table that depends on one of the set through ON DELETE CASCADE, transitively:
// replacing or exporting a parent without them would wipe them or leave them dangling.
func (sc *bmgrSchema) closure(set map[string]bool) map[string]bool {
	out := map[string]bool{}
	for t := range set {
		out[t] = true
	}
	for changed := true; changed; {
		changed = false
		for _, fk := range sc.FKs {
			if fk.Rule == 'c' && out[fk.Parent] && !out[fk.Child] && sc.has(fk.Child) {
				out[fk.Child] = true
				changed = true
			}
		}
	}
	return out
}

func bmgrQ(t string) string { return pgx.Identifier{"public", t}.Sanitize() }

// bmgrUserFilters are the per-table WHERE clauses that keep only the chosen users' rows. users itself
// is filtered by id, and every table in the set that cascades from users by its user column.
func (sc *bmgrSchema) userFilters(set map[string]bool) map[string]string {
	f := map[string]string{}
	if set["users"] {
		f["users"] = pgx.Identifier{"id"}.Sanitize() + " = ANY($1)"
	}
	for _, fk := range sc.FKs {
		if fk.Parent == "users" && fk.Rule == 'c' && !fk.Composite && set[fk.Child] && set["users"] {
			if f[fk.Child] == "" {
				f[fk.Child] = pgx.Identifier{fk.Col}.Sanitize() + " = ANY($1)"
			}
		}
	}
	return f
}

// bmgrExportTable streams one table as JSON lines into w and returns the row count. where, when
// non-empty, takes the user ids as $1.
func bmgrExportTable(ctx context.Context, tx pgx.Tx, table, where string, ids []int64, w io.Writer) (int64, error) {
	q := `SELECT to_jsonb(t)::text FROM ` + bmgrQ(table) + ` t`
	var args []any
	if where != "" {
		q += ` WHERE ` + where
		args = append(args, ids)
	}
	if table == "settings" {
		if where == "" {
			q += ` WHERE `
		} else {
			q += ` AND `
		}
		args = append(args, bmgrNoExportSettings)
		q += fmt.Sprintf(`t.key <> ALL($%d)`, len(args))
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	bw := bufio.NewWriterSize(w, 256<<10)
	var n int64
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return n, err
		}
		if _, err := bw.WriteString(line + "\n"); err != nil {
			return n, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	return n, bw.Flush()
}

// bmgrRestoreOpts steer a database restore.
type bmgrRestoreOpts struct {
	Tables    map[string]bool // tables to replace (already closed over cascades)
	Columns   map[string][]string
	OldURL    string // the archive site's address ("" = no rewrite)
	NewURL    string
	Me        int64                                 // the administrator running the restore
	Open      func(table string) (io.Reader, error) // the table's JSON lines from the archive; nil when the archive lacks it
	Progress  func(i, n int, table string)
	LogWriter func(string)
	Reseal    *bmgrResealer // re-encrypts sealed secrets from the archive's site secret to this site's (nil: none)
}

// bmgrRestoreDB replaces the chosen tables from the archive inside one transaction. Foreign keys are
// made deferrable for its duration (and back afterwards); orphans left by a partial restore are
// repaired by the key's own ON DELETE rule before the constraints are checked.
func bmgrRestoreDB(ctx context.Context, tx pgx.Tx, sc *bmgrSchema, o bmgrRestoreOpts) error {
	var tables []string
	for t := range o.Tables {
		if sc.has(t) {
			tables = append(tables, t)
		}
	}
	sort.Strings(tables)

	var touched []bmgrFK // every FK with a side in the set
	for _, fk := range sc.FKs {
		if o.Tables[fk.Child] || o.Tables[fk.Parent] {
			touched = append(touched, fk)
		}
	}
	// A key from a table outside the set to a restored one with ON DELETE SET NULL / SET DEFAULT would fire
	// during the DELETE below (PostgreSQL never defers referential actions), wiping every link even though the
	// parent rows come back with the same ids: restoring Users alone would orphan every post's author. Such keys
	// are dropped for the restore and added back afterwards (which validates them); links whose parent is
	// really gone are repaired by the key's own rule first.
	type bridge struct {
		fk  bmgrFK
		def string
	}
	var bridges []bridge
	bridged := map[string]bool{}
	for _, fk := range touched {
		if !o.Tables[fk.Parent] || o.Tables[fk.Child] || (fk.Rule != 'n' && fk.Rule != 'd') || fk.Composite || !sc.has(fk.Child) {
			continue
		}
		var def string
		if err := tx.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1 AND conrelid = $2::regclass`,
			fk.Name, bmgrQ(fk.Child)).Scan(&def); err != nil {
			return fmt.Errorf("reading %s: %w", fk.Name, err)
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE `+bmgrQ(fk.Child)+` DROP CONSTRAINT `+pgx.Identifier{fk.Name}.Sanitize()); err != nil {
			return fmt.Errorf("lifting %s: %w", fk.Name, err)
		}
		bridges = append(bridges, bridge{fk, def})
		bridged[fk.Child+"\x00"+fk.Name] = true
	}
	for _, fk := range touched {
		if fk.Rule == 'r' || bridged[fk.Child+"\x00"+fk.Name] {
			continue
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE `+bmgrQ(fk.Child)+` ALTER CONSTRAINT `+pgx.Identifier{fk.Name}.Sanitize()+` DEFERRABLE INITIALLY DEFERRED`); err != nil {
			return fmt.Errorf("deferring %s: %w", fk.Name, err)
		}
	}

	// Empty the tables, keeping this site's own options.
	for _, t := range tables {
		if t == "settings" {
			if _, err := tx.Exec(ctx, `DELETE FROM `+bmgrQ(t)+` WHERE key <> ALL($1)`, bmgrKeepSettings); err != nil {
				return fmt.Errorf("clearing settings: %w", err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+bmgrQ(t)); err != nil {
			return fmt.Errorf("clearing %s: %w", t, err)
		}
	}

	// Migration 0048 made lower(login) and lower(email) unique, but only on a site without accounts that
	// differ by case. An archive of such a site (or of a version before 0048) may hold them: the indexes are
	// lifted for the restore and put back afterwards the way 0048 does, unique when the restored rows allow.
	var liftedCI []string
	if o.Tables["users"] && sc.has("users") {
		var err error
		if liftedCI, err = bmgrLiftUserCIIndexes(ctx, tx); err != nil {
			return err
		}
	}

	rewrite := func(s string) string { return s }
	if o.OldURL != "" && o.NewURL != "" && o.OldURL != o.NewURL {
		oldEsc, newEsc := strings.ReplaceAll(o.OldURL, "/", `\/`), strings.ReplaceAll(o.NewURL, "/", `\/`)
		rewrite = func(s string) string {
			return strings.ReplaceAll(strings.ReplaceAll(s, o.OldURL, o.NewURL), oldEsc, newEsc)
		}
	}
	keep := map[string]bool{}
	for _, k := range bmgrKeepSettings {
		keep[k] = true
	}
	// An archive made before menu_locations existed (0.1.7 and older) holds each menu's location in a
	// menus.location column: it is collected while the menus are read and becomes menu_locations below.
	legacyLocs := o.Tables["menu_locations"] && contains(o.Columns["menus"], "location")
	var menuLocs []bmgrLegacyMenuLoc
	inArchive := map[string]bool{}

	for i, t := range tables {
		if o.Progress != nil {
			o.Progress(i, len(tables), t)
		}
		if o.Open == nil {
			continue
		}
		r, err := o.Open(t)
		if err != nil {
			return err
		}
		if r == nil {
			continue
		}
		inArchive[t] = true
		cols := bmgrWritableColumns(sc, t, o.Columns[t])
		if len(cols) == 0 {
			continue
		}
		list := make([]string, len(cols))
		for j, c := range cols {
			list[j] = pgx.Identifier{c}.Sanitize()
		}
		stmt := `INSERT INTO ` + bmgrQ(t) + ` (` + strings.Join(list, ",") + `) SELECT ` + strings.Join(list, ",") +
			` FROM jsonb_populate_recordset(NULL::` + bmgrQ(t) + `, $1::jsonb)`
		sc2 := bufio.NewReaderSize(r, 1<<20)
		batch := make([]string, 0, 500)
		size := 0
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			_, err := tx.Exec(ctx, stmt, "["+strings.Join(batch, ",")+"]")
			batch, size = batch[:0], 0
			if err != nil {
				return fmt.Errorf("restoring %s: %w", t, err)
			}
			return nil
		}
		for {
			line, err := sc2.ReadString('\n')
			line = strings.TrimSpace(line)
			if line != "" {
				if t == "settings" {
					var k struct {
						Key string `json:"key"`
					}
					if json.Unmarshal([]byte(line), &k) == nil && keep[k.Key] {
						line = ""
					}
				}
				if line != "" && t == "menus" && legacyLocs {
					var m bmgrLegacyMenuLoc
					if json.Unmarshal([]byte(line), &m) == nil && strings.TrimSpace(m.Location) != "" {
						menuLocs = append(menuLocs, m)
					}
				}
				if line != "" {
					batch = append(batch, rewrite(o.Reseal.line(line)))
					size += len(line)
					if len(batch) >= 500 || size > 8<<20 {
						if ferr := flush(); ferr != nil {
							return ferr
						}
					}
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("reading %s: %w", t, err)
			}
		}
		if err := flush(); err != nil {
			return err
		}
	}

	if legacyLocs && !inArchive["menu_locations"] && len(menuLocs) > 0 {
		if err := bmgrRestoreLegacyMenuLocations(ctx, tx, menuLocs); err != nil {
			return err
		}
	}

	if len(liftedCI) > 0 {
		if err := bmgrRestoreUserCIIndexes(ctx, tx, liftedCI, o.LogWriter); err != nil {
			return err
		}
	}

	// This site's address wins over the archive's.
	if o.Tables["settings"] && o.NewURL != "" {
		// kept rows were never deleted, but a site without them still gets its address.
		for _, k := range []string{"siteurl", "home"} {
			if _, err := tx.Exec(ctx, `INSERT INTO settings(key, value) VALUES ($1, to_jsonb($2::text)) ON CONFLICT (key) DO NOTHING`, k, o.NewURL); err != nil {
				return err
			}
		}
	}

	// Content whose author was not restored goes to the administrator running the restore.
	if o.Tables["posts"] && !o.Tables["users"] && o.Me > 0 {
		if _, err := tx.Exec(ctx, `UPDATE posts SET author_id = $1 WHERE author_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = posts.author_id)`, o.Me); err != nil {
			return err
		}
	}
	// Orphans of a partial restore follow the key's own ON DELETE rule.
	for _, fk := range touched {
		if fk.Composite || !sc.has(fk.Child) {
			continue
		}
		orphan := ` FROM ` + bmgrQ(fk.Child) + ` c WHERE c.` + pgx.Identifier{fk.Col}.Sanitize() + ` IS NOT NULL AND NOT EXISTS (SELECT 1 FROM ` +
			bmgrQ(fk.Parent) + ` p WHERE p.` + pgx.Identifier{fk.ParentCol}.Sanitize() + ` = c.` + pgx.Identifier{fk.Col}.Sanitize() + `)`
		switch {
		case fk.Rule == 'n' || (fk.Rule != 'c' && fk.Nullable):
			_, err := tx.Exec(ctx, `UPDATE `+bmgrQ(fk.Child)+` SET `+pgx.Identifier{fk.Col}.Sanitize()+` = NULL WHERE ctid IN (SELECT c.ctid`+orphan+`)`)
			if err != nil {
				return fmt.Errorf("repairing %s.%s: %w", fk.Child, fk.Col, err)
			}
		case fk.Rule == 'c':
			if _, err := tx.Exec(ctx, `DELETE FROM `+bmgrQ(fk.Child)+` WHERE ctid IN (SELECT c.ctid`+orphan+`)`); err != nil {
				return fmt.Errorf("repairing %s.%s: %w", fk.Child, fk.Col, err)
			}
		}
	}

	// The keys lifted for the restore come back in their original form (validated: orphans were just repaired).
	for _, b := range bridges {
		if _, err := tx.Exec(ctx, `ALTER TABLE `+bmgrQ(b.fk.Child)+` ADD CONSTRAINT `+pgx.Identifier{b.fk.Name}.Sanitize()+` `+b.def); err != nil {
			return fmt.Errorf("restoring %s: %w", b.fk.Name, err)
		}
	}

	// The next generated id must clear the restored ones.
	for _, t := range tables {
		for _, col := range sc.Serials[t] {
			qc := pgx.Identifier{col}.Sanitize()
			if _, err := tx.Exec(ctx, `SELECT setval(pg_get_serial_sequence($1, $2), GREATEST(COALESCE((SELECT MAX(`+qc+`) FROM `+bmgrQ(t)+`), 0), 1), (SELECT MAX(`+qc+`) FROM `+bmgrQ(t)+`) IS NOT NULL)`, bmgrQ(t), col); err != nil {
				return fmt.Errorf("resetting the %s.%s sequence: %w", t, col, err)
			}
		}
	}

	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		return fmt.Errorf("the restored data breaks a relation: %w", err)
	}
	for _, fk := range touched {
		if fk.Rule == 'r' || bridged[fk.Child+"\x00"+fk.Name] {
			continue
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE `+bmgrQ(fk.Child)+` ALTER CONSTRAINT `+pgx.Identifier{fk.Name}.Sanitize()+` NOT DEFERRABLE`); err != nil {
			return fmt.Errorf("restoring %s: %w", fk.Name, err)
		}
	}
	return nil
}

// bmgrWritableColumns are the columns present in both the archive and the live table, minus generated ones.
func bmgrWritableColumns(sc *bmgrSchema, table string, archive []string) []string {
	in := map[string]bool{}
	for _, c := range archive {
		in[c] = true
	}
	var out []string
	for _, c := range sc.Columns[table] {
		if !c.Gen && (len(archive) == 0 || in[c.Name]) {
			out = append(out, c.Name)
		}
	}
	return out
}

// bmgrLegacyMenuLoc is a menus row of an archive from before menu_locations: its single location.
type bmgrLegacyMenuLoc struct {
	ID       int64  `json:"id"`
	Location string `json:"location"`
}

// bmgrRestoreLegacyMenuLocations maps the old menus.location column into menu_locations, as migration
// 0038 does: a location claimed by several menus goes to the oldest one.
func bmgrRestoreLegacyMenuLocations(ctx context.Context, tx pgx.Tx, locs []bmgrLegacyMenuLoc) error {
	sort.SliceStable(locs, func(i, j int) bool { return locs[i].ID < locs[j].ID })
	for _, l := range locs {
		if _, err := tx.Exec(ctx, `INSERT INTO menu_locations (location, menu_id) SELECT $1, id FROM menus WHERE id = $2
			ON CONFLICT (location) DO NOTHING`, strings.TrimSpace(l.Location), l.ID); err != nil {
			return fmt.Errorf("restoring the menu locations: %w", err)
		}
	}
	return nil
}

// bmgrUserCIColumns are the users columns migration 0048 indexes case-insensitively: the unique index
// and the plain lookup index (0032) a site with case duplicates keeps instead.
var bmgrUserCIColumns = []struct{ Col, Unique, Plain string }{
	{"login", "users_lower_login_key", "users_lower_login"},
	{"email", "users_lower_email_key", "users_lower_email"},
}

// bmgrLiftUserCIIndexes drops the case-insensitive unique indexes of users for a restore (a plain lookup
// index takes their place) and returns the columns bmgrRestoreUserCIIndexes settles afterwards: all of
// them, so a site left non-unique by older data becomes unique again once the restored accounts allow it.
func bmgrLiftUserCIIndexes(ctx context.Context, tx pgx.Tx) ([]string, error) {
	var cols []string
	for _, ci := range bmgrUserCIColumns {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+ci.Unique).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			if _, err := tx.Exec(ctx, `DROP INDEX `+pgx.Identifier{"public", ci.Unique}.Sanitize()); err != nil {
				return nil, fmt.Errorf("lifting %s: %w", ci.Unique, err)
			}
		}
		// The plain lookup index serves the login form while the unique one is gone (and stays when it cannot come back).
		if _, err := tx.Exec(ctx, `CREATE INDEX IF NOT EXISTS `+pgx.Identifier{ci.Plain}.Sanitize()+` ON users (lower(`+pgx.Identifier{ci.Col}.Sanitize()+`))`); err != nil {
			return nil, fmt.Errorf("indexing users.%s: %w", ci.Col, err)
		}
		cols = append(cols, ci.Col)
	}
	return cols, nil
}

// bmgrRestoreUserCIIndexes puts the lifted unique indexes back as migration 0048 does: unique when the
// restored accounts have no case duplicates in that column, otherwise the plain lookup index stays.
func bmgrRestoreUserCIIndexes(ctx context.Context, tx pgx.Tx, cols []string, logf func(string)) error {
	for _, ci := range bmgrUserCIColumns {
		if !contains(cols, ci.Col) {
			continue
		}
		col := pgx.Identifier{ci.Col}.Sanitize()
		var dup bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users GROUP BY lower(`+col+`) HAVING count(*) > 1)`).Scan(&dup); err != nil {
			return err
		}
		if dup {
			if logf != nil {
				logf("note: the backup has accounts whose " + ci.Col + " differs only by case; " + ci.Col + " stays non-unique regardless of case")
			}
			continue
		}
		if _, err := tx.Exec(ctx, `CREATE UNIQUE INDEX `+pgx.Identifier{ci.Unique}.Sanitize()+` ON users (lower(`+col+`))`); err != nil {
			return fmt.Errorf("restoring %s: %w", ci.Unique, err)
		}
		if _, err := tx.Exec(ctx, `DROP INDEX IF EXISTS `+pgx.Identifier{"public", ci.Plain}.Sanitize()); err != nil {
			return err
		}
	}
	return nil
}
