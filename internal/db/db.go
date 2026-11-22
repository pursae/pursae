// Package db owns the PostgreSQL pool and schema migrations.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"wpgo/internal/config"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Connect opens a pool and waits for the database to accept connections.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := PoolConfig(url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = pool.Ping(ctx)
		if err == nil {
			return pool, nil
		}
		// A cancelled ctx (Ctrl-C, docker stop while PostgreSQL is still starting) ends the wait at
		// once instead of retrying until the deadline and being reported as unreachable.
		if ctx.Err() != nil {
			pool.Close()
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("database not reachable: %w", err)
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			pool.Close()
			return nil, err
		}
	}
}

// QueryTracer, when set before Connect, is installed on every connection of the pool (the slow
// query log and counter of internal/metrics). Nil is the default: no tracer, no per-query work.
var QueryTracer pgx.QueryTracer

// PoolConfig parses url and applies the pool defaults below to every setting the URL does not
// carry itself (pool_max_conns, pool_min_conns, pool_max_conn_lifetime, pool_max_conn_idle_time,
// pool_health_check_period — pgx's own query parameters).
//
// pgx alone would allow max(4, NumCPU) connections, too few for a site whose requests, scheduler
// and background writers share one pool. The cap of 40 stays well inside PostgreSQL's default
// max_connections (100) so a second instance or a backup still gets a connection.
func PoolConfig(rawURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(rawURL)
	if err != nil {
		return nil, err
	}
	set := func(param string) bool { return poolParamSet(rawURL, param) }
	if QueryTracer != nil {
		cfg.ConnConfig.Tracer = QueryTracer
	}
	if !set("pool_max_conns") {
		cfg.MaxConns = defaultMaxConns(runtime.NumCPU())
	}
	if !set("pool_min_conns") {
		cfg.MinConns = 2
	}
	if cfg.MinConns > cfg.MaxConns {
		cfg.MinConns = cfg.MaxConns
	}
	if !set("pool_max_conn_lifetime") {
		cfg.MaxConnLifetime = time.Hour
	}
	if !set("pool_max_conn_lifetime_jitter") {
		cfg.MaxConnLifetimeJitter = 5 * time.Minute // instances started together do not reconnect together
	}
	if !set("pool_max_conn_idle_time") {
		cfg.MaxConnIdleTime = 15 * time.Minute
	}
	if !set("pool_health_check_period") {
		cfg.HealthCheckPeriod = 30 * time.Second
	}
	return cfg, nil
}

// poolParamSet reports whether param is an exact connection-string key. A substring check would,
// for example, mistake xpool_max_conns for pool_max_conns and suppress the intended default.
func poolParamSet(raw, param string) bool {
	if u, err := url.Parse(raw); err == nil && u.RawQuery != "" {
		_, ok := u.Query()[param]
		return ok
	}
	// pgx also accepts the PostgreSQL keyword/value connection-string form.
	for _, field := range strings.Fields(raw) {
		key, _, ok := strings.Cut(field, "=")
		if ok && key == param {
			return true
		}
	}
	return false
}

// defaultMaxConns is 4 per CPU, at least 10 and at most 40.
func defaultMaxConns(cpus int) int32 {
	return int32(min(max(10, 4*cpus), 40))
}

// migrationSource is where Migrate and Pending read migrations from: the embedded files, swapped
// for a fixture only by tests (which must restore it and not run in parallel).
var migrationSource fs.FS = migrationFS

// noTransactionMarker, as the first line of a migration, makes Migrate run it outside a
// transaction (see Migrate).
const noTransactionMarker = "-- wpgo:no-transaction"

// migrateLockPoll is how often Migrate retries the advisory lock another instance holds.
var migrateLockPoll = 500 * time.Millisecond

// Migrate applies every embedded migration that has not run yet, each in its own transaction with
// a bounded lock_timeout (see applyMigration). A session-level advisory lock keeps concurrent
// instances from migrating twice.
//
// Index builds on large existing tables (posts, postmeta, users, usermeta, comments, commentmeta,
// terms, term_taxonomy, term_relationships, termmeta) must not run in a transactional migration: a
// plain CREATE INDEX blocks every write to the table for the whole build. Put each such index in its
// own migration file whose first line is "-- wpgo:no-transaction" and whose only statement is
// CREATE [UNIQUE] INDEX CONCURRENTLY IF NOT EXISTS ... . Such a file:
//   - holds exactly one statement: pgx sends a multi-statement string as one implicit transaction,
//     where CONCURRENTLY fails; use more files for more statements;
//   - must be idempotent (IF NOT EXISTS), because a crash between the build and the
//     schema_migrations insert runs it again;
//   - may leave an INVALID index behind when a build fails; Migrate drops that index
//     (DROP INDEX CONCURRENTLY) before building it again.
//
// Before anything is applied the schema guard runs (GuardSchema with BinaryVersion): a binary below
// the floor a newer release recorded fails with *BinaryTooOldError. Afterwards the floor
// (MinCompatibleBinary) and BinaryVersion are recorded in schema_meta, never lowering either.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	// Poll with pg_try_advisory_lock rather than blocking in pg_advisory_lock: a blocked statement
	// holds a snapshot, CREATE INDEX CONCURRENTLY in the instance that does migrate waits for every
	// older snapshot, and that instance holds the advisory lock the waiter needs, so PostgreSQL
	// would abort one side as a deadlock and fail the other instance's startup.
	const lockID = 7_356_001
	for {
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockID).Scan(&locked); err != nil {
			return nil, err
		}
		if locked {
			break
		}
		if err := sleepCtx(ctx, migrateLockPoll); err != nil {
			return nil, err
		}
	}
	// Best effort: an unlock that fails means the connection is broken, and pgx closes a broken
	// connection on Release, which ends the session and its lock.
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID) //nolint:errcheck // see above

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	// A binary below the floor a newer one recorded must not touch the schema (schema_guard.go).
	if _, err := GuardSchema(ctx, conn, BinaryVersion); err != nil {
		return nil, err
	}

	names, err := fs.Glob(migrationSource, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	settings := loadMigrationSettings()
	var applied []string
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		sql, err := fs.ReadFile(migrationSource, name)
		if err != nil {
			return applied, err
		}
		if isNoTransaction(string(sql)) {
			err = applyMigrationNoTx(ctx, conn, version, string(sql), settings)
		} else {
			err = applyMigration(ctx, conn, version, string(sql), settings)
		}
		if err != nil {
			return applied, err
		}
		applied = append(applied, version)
	}
	if err := recordSchemaMeta(ctx, conn, migrationFloor, BinaryVersion); err != nil {
		return applied, fmt.Errorf("schema_meta: %w", err)
	}
	return applied, nil
}

// migrationFloor is what Migrate records as min_binary_version: MinCompatibleBinary, swapped only
// by tests.
var migrationFloor = MinCompatibleBinary

// Migration timeouts. A DDL statement such as ALTER TABLE needs an ACCESS EXCLUSIVE lock; during a
// rolling deploy the old instance may still run a long query on that table, and while the ALTER
// waits every later reader and writer queues behind it, so live traffic stalls. lock_timeout bounds
// that wait; the migration is then retried in a fresh transaction a few times before startup fails.
//
// statement_timeout is off (0) by default and set explicitly, so a role- or database-level default
// cannot cut off a data backfill that legitimately runs long on a large site.
const (
	defaultMigrationLockTimeout = 5 * time.Second
	defaultMigrationAttempts    = 5
)

// migrationRetryBackoff is the wait before the second attempt; it doubles per attempt up to
// maxMigrationRetryBackoff. Tests shorten it.
var (
	migrationRetryBackoff    = time.Second
	maxMigrationRetryBackoff = 8 * time.Second
)

// migrationSettings are the per-migration limits applyMigration uses.
type migrationSettings struct {
	lockTimeout      time.Duration // 0 disables (PostgreSQL's own meaning of lock_timeout = 0)
	statementTimeout time.Duration // 0 disables
	attempts         int           // total tries on lock_not_available, at least 1
}

// loadMigrationSettings reads PURSAE_MIGRATE_LOCK_TIMEOUT and PURSAE_MIGRATE_STATEMENT_TIMEOUT (Go
// durations such as "5s" or "30m"). It lives in this package rather than in main so every caller
// of Migrate (serve, wpgo migrate, theme previews, tests) gets the same limits. An invalid or
// negative value falls back to the default.
func loadMigrationSettings() migrationSettings {
	return migrationSettings{
		lockTimeout:      envDuration("PURSAE_MIGRATE_LOCK_TIMEOUT", defaultMigrationLockTimeout),
		statementTimeout: envDuration("PURSAE_MIGRATE_STATEMENT_TIMEOUT", 0),
		attempts:         defaultMigrationAttempts,
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(config.Env(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// applyMigration runs one migration and records it in schema_migrations, both in one transaction.
// When the migration cannot get a lock within s.lockTimeout (SQLSTATE 55P03) the transaction has
// already rolled back, so the whole file is retried in a new one, with backoff, up to s.attempts.
func applyMigration(ctx context.Context, conn *pgxpool.Conn, version, sql string, s migrationSettings) error {
	attempts := max(s.attempts, 1)
	backoff := migrationRetryBackoff
	for attempt := 1; ; attempt++ {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// SET does not take $1 parameters; the values are formatted durations, not input. SET LOCAL
			// ends with the transaction, so the pooled connection keeps its own settings. Both run
			// before the file, so a migration may still override them.
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", s.lockTimeout.Milliseconds())); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", s.statementTimeout.Milliseconds())); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, version)
			return err
		})
		if err == nil {
			return nil
		}
		if !isLockNotAvailable(err) {
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if attempt >= attempts {
			return fmt.Errorf("migration %s: lock_timeout exceeded after %d attempts (another session holds a conflicting lock): %w", version, attempt, err)
		}
		if err := sleepCtx(ctx, backoff); err != nil {
			return fmt.Errorf("migration %s: %w", version, err)
		}
		backoff = min(backoff*2, maxMigrationRetryBackoff)
	}
}

// isNoTransaction reports whether a migration's first line is the no-transaction marker.
func isNoTransaction(sql string) bool {
	first, _, _ := strings.Cut(sql, "\n")
	return strings.TrimSpace(first) == noTransactionMarker
}

// concurrentIndexName is the index a CREATE [UNIQUE] INDEX CONCURRENTLY IF NOT EXISTS migration
// builds, or "" for any other statement.
var concurrentIndexName = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+IF\s+NOT\s+EXISTS\s+([A-Za-z_][A-Za-z0-9_$.]*|"[^"]+")`)

var usesConcurrently = regexp.MustCompile(`(?i)\bCONCURRENTLY\b`)

// stripLineComments drops "--" comment lines (the marker among them) so a statement's first word
// can be matched.
func stripLineComments(sql string) string {
	var b strings.Builder
	for line := range strings.Lines(sql) {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			b.WriteString(line)
		}
	}
	return b.String()
}

// applyMigrationNoTx runs a "-- wpgo:no-transaction" migration (one statement, see Migrate) on
// conn outside a transaction, then records it.
//
// The timeouts are session-level here (SET LOCAL needs a transaction) and RESET afterwards, since
// conn goes back to the pool. lock_timeout is not applied to a CONCURRENTLY statement: its waits for
// older transactions are lock waits too, so a short timeout would fail the build on any busy site,
// and its SHARE UPDATE EXCLUSIVE lock does not block reads or writes while it waits.
func applyMigrationNoTx(ctx context.Context, conn *pgxpool.Conn, version, sql string, s migrationSettings) error {
	stmt := stripLineComments(sql)
	lockTimeout := s.lockTimeout
	if usesConcurrently.MatchString(stmt) {
		lockTimeout = 0
	}
	defer conn.Exec(context.Background(), `RESET lock_timeout`)
	defer conn.Exec(context.Background(), `RESET statement_timeout`)
	if _, err := conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = '%dms'", lockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("migration %s: %w", version, err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = '%dms'", s.statementTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("migration %s: %w", version, err)
	}

	attempts := max(s.attempts, 1)
	backoff := migrationRetryBackoff
	for attempt := 1; ; attempt++ {
		err := execNoTx(ctx, conn, stmt, sql)
		if err == nil {
			break
		}
		if !isLockNotAvailable(err) {
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if attempt >= attempts {
			return fmt.Errorf("migration %s: lock_timeout exceeded after %d attempts (another session holds a conflicting lock): %w", version, attempt, err)
		}
		if err := sleepCtx(ctx, backoff); err != nil {
			return fmt.Errorf("migration %s: %w", version, err)
		}
		backoff = min(backoff*2, maxMigrationRetryBackoff)
	}
	// ON CONFLICT: the statement is idempotent and may have run before a crash; only the record
	// counts here.
	if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1) ON CONFLICT DO NOTHING`, version); err != nil {
		return fmt.Errorf("migration %s: %w", version, err)
	}
	return nil
}

// execNoTx runs one no-transaction statement. When it builds an index concurrently and an earlier,
// failed build left that index INVALID, the invalid index is dropped first; IF NOT EXISTS would
// otherwise keep it and never build a usable one.
func execNoTx(ctx context.Context, conn *pgxpool.Conn, stmt, sql string) error {
	if m := concurrentIndexName.FindStringSubmatch(stmt); m != nil {
		var invalid bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_index WHERE indexrelid = to_regclass($1) AND NOT indisvalid)`, m[1]).Scan(&invalid); err != nil {
			return err
		}
		if invalid {
			// m[1] is an identifier from an embedded migration file, not input.
			if _, err := conn.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+m[1]); err != nil {
				return err
			}
		}
	}
	_, err := conn.Exec(ctx, sql)
	return err
}

// isLockNotAvailable reports whether err is PostgreSQL's lock_not_available (55P03), which
// lock_timeout raises.
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// sleepCtx waits d, or returns ctx.Err() as soon as ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ConnectSchema opens a pool on the database of base whose connections see only the given schema
// (search_path): a second, separate site in the same database, migrated with Migrate like the
// first (theme previews). The schema must exist.
func ConnectSchema(ctx context.Context, base *pgxpool.Config, schema string, maxConns int32) (*pgxpool.Pool, error) {
	cfg := base.Copy()
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize()
	cfg.MaxConns, cfg.MinConns = maxConns, 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Versions lists the embedded migrations by version (file name without .sql), in the order
// Migrate applies them. Compare with schema_migrations to see which are applied.
func Versions() ([]string, error) {
	names, err := fs.Glob(migrationSource, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql"))
	}
	return out, nil
}

// Pending counts the embedded migrations the database has not applied yet (readiness checks).
// A database that was never migrated (no schema_migrations table) has all of them pending.
func Pending(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	want, err := Versions()
	if err != nil {
		return 0, err
	}
	var applied int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ANY($1)`, want).Scan(&applied)
	if IsUndefinedTable(err) {
		return len(want), nil
	}
	if err != nil {
		return 0, err
	}
	return len(want) - applied, nil
}

// IsUndefinedTable reports a Postgres "relation does not exist" error (SQLSTATE 42P01), e.g. a
// query on a database that was never migrated.
func IsUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}
