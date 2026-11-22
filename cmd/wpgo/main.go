// Command wpgo runs WordPress Go (Pursae): a WordPress-compatible CMS in a single binary. The
// same binary answers to the names wpgo and pursae (release builds are pursae-<os>-<arch>);
// besides serve and the operational commands below it carries a WP-CLI style command line
// (`pursae help`, internal/server/cli.go).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // site time zones work on minimal images

	"github.com/jackc/pgx/v5/pgxpool"

	"wpgo/internal/config"
	"wpgo/internal/coreupdate"
	"wpgo/internal/db"
	"wpgo/internal/plugins/pkgtool"
	"wpgo/internal/server"
)

func main() {
	prog := progName(os.Args[0])
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix(prog + ": ")
	cmd := "serve" // no arguments: the container's default
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	config.WarnDeprecatedEnv() // WPGO_* settings still work until 1.0; name each one's PURSAE_* successor
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// In a container the server runs the binary Launch installed in PURSAE_BIN_DIR, which a core
	// update replaces; `docker exec … pursae <command>` starts the image's (possibly older) binary.
	// Commands that touch the site run the installed one when it is newer, so the CLI never
	// works with older code than the server against the schema the server migrated.
	if usesInstalledBinary(cmd) && os.Getenv(reexecEnv) == "" && os.Getenv(legacyReexecEnv) == "" {
		if exe, err := executable(); err == nil {
			if bin := coreupdate.Installed(exe, config.Env("BIN_DIR"), server.Version); bin != "" {
				os.Setenv(reexecEnv, "1")       // never loop
				os.Setenv(legacyReexecEnv, "1") // an installed release before 0.5.0 reads only this name
				err := coreupdate.Exec(bin, os.Args)
				log.Printf("running the installed %s failed (%v); continuing with %s", bin, err, server.Version)
			}
		}
	}

	// `<cmd> --help` / `-h` on the commands handled below shows their help instead of running
	// them (`setup-token --help` must not replace the setup token, `migrate -h` must not migrate).
	if topic := helpTopic(os.Args[1:]); topic != nil {
		os.Exit(runCLI(ctx, cfg, prog, append([]string{"help"}, topic...)))
	}

	switch cmd {
	case "serve":
		if exe, err := executable(); err == nil {
			coreupdate.Guard(exe, server.Version) // a failed core update goes back to the previous binary here
		}
		if err := serve(ctx, cfg); err != nil {
			log.Fatal(err)
		}
	case "migrate":
		pool, err := db.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Fatal(err)
		}
		defer pool.Close()
		applied, err := db.Migrate(ctx, pool)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("applied %d migration(s) %v\n", len(applied), applied)
	case "launch": // container entrypoint: run the installed binary (see internal/coreupdate.Launch)
		exe, err := executable()
		if err != nil {
			log.Fatal(err)
		}
		dir := config.Env("BIN_DIR")
		args := os.Args[2:]
		if len(args) == 0 {
			args = []string{"serve"}
		}
		log.Fatal(coreupdate.Launch(exe, dir, args))
	case "version":
		fmt.Println("Pursae", server.Version)
	case "healthcheck": // container HEALTHCHECK: GET /healthz on PURSAE_ADDR (healthcheck.go)
		if err := healthcheck(cfg.Addr, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, prog+" healthcheck:", err)
			os.Exit(1)
		}
	case "plugin": // plugin tooling: new, build, keygen, pack, verify (internal/plugins/pkgtool); other verbs: the CLI
		if len(os.Args) > 2 && server.IsPkgtoolVerb(os.Args[2]) {
			if err := pkgtool.Run(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, prog+" plugin:", err)
				os.Exit(1)
			}
			return
		}
		os.Exit(runCLI(ctx, cfg, prog, os.Args[1:]))
	case "mcp": // stdio MCP bridge to a site's /wp-json/pursae/v1/mcp (mcp.go)
		if err := runMCP(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, prog+" mcp:", err)
			os.Exit(1)
		}
	case "adminsec": // AdminSecGo recovery: disable-hide-login, reset-user <login>, unlock-all, deactivate
		pool, err := connectGuarded(ctx, cfg)
		if err != nil {
			log.Fatal(err)
		}
		defer pool.Close()
		out, err := server.AdminSecCLI(ctx, pool, os.Args[2:])
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(renameUsage(out, prog))
	case "setup-token": // print the install URL with the setup token (a new token unless PURSAE_SETUP_TOKEN is set)
		if err := setupToken(ctx, cfg, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, prog+" setup-token:", err)
			os.Exit(1)
		}
	case "headless": // Pursae Headless: export / import entries as NDJSON
		pool, err := connectGuarded(ctx, cfg)
		if err != nil {
			log.Fatal(err)
		}
		defer pool.Close()
		if err := server.HeadlessCLI(ctx, pool, os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, prog+" headless:", renameUsage(err.Error(), prog))
			os.Exit(1)
		}
	default: // help, --help, -h and every other noun: the WP-CLI style command line (internal/server/cli.go)
		os.Exit(runCLI(ctx, cfg, prog, os.Args[1:]))
	}
}

// reexecEnv marks a process that already switched to the installed binary.
const reexecEnv = "PURSAE_CLI_REEXEC"

// legacyReexecEnv is reexecEnv's name before 0.5.0: an older image's CLI sets it when it runs a newer
// installed binary, and an older installed binary reads only it.
const legacyReexecEnv = "WPGO_CLI_REEXEC"

// usesInstalledBinary reports whether cmd should run the newer binary in PURSAE_BIN_DIR: every
// command except the ones that pick their binary themselves (serve, launch), the container
// HEALTHCHECK and the MCP bridge, which do not touch the database.
func usesInstalledBinary(cmd string) bool {
	switch cmd {
	case "serve", "launch", "healthcheck", "mcp":
		return false
	}
	return true
}

// mainCommands are the commands main runs itself (help entries in internal/server/cli.go).
var mainCommands = map[string]bool{
	"serve": true, "migrate": true, "launch": true, "version": true, "healthcheck": true,
	"mcp": true, "adminsec": true, "setup-token": true, "headless": true,
}

// helpTopic returns the help topic when args ask for the help of a command main handles itself
// (`serve --help`, `adminsec -h`, `plugin pack --help`), nil otherwise. Every other command
// line goes to the CLI, which handles --help on its own.
func helpTopic(args []string) []string {
	isHelp := func(s string) bool { return s == "--help" || s == "-h" }
	switch {
	case len(args) >= 2 && mainCommands[args[0]] && isHelp(args[1]):
		return args[:1]
	case len(args) >= 3 && args[0] == "plugin" && server.IsPkgtoolVerb(args[1]) && isHelp(args[2]):
		return args[:2]
	}
	return nil
}

// runCLI runs the WP-CLI style command line and returns its exit code. os.Exit skips deferred
// calls, so the CLI closes its own pool before returning.
func runCLI(ctx context.Context, cfg config.Config, prog string, args []string) int {
	// Commands report through their own output; the server's informational log lines (plugin
	// runtime loads, media index …) would only clutter stderr. Warnings and errors still show.
	slog.SetLogLoggerLevel(slog.LevelWarn)
	return server.CLI(ctx, cfg, prog, args, os.Stdin, os.Stdout, os.Stderr)
}

// progName is the program name shown in usage and log lines, from how the binary was invoked:
// "pursae" for pursae, pursae.exe and the release names pursae-<os>-<arch>; "wpgo" for anything
// containing wpgo (the Docker image, make build, go run). Other names fall back to "pursae".
func progName(arg0 string) string {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(arg0)), ".exe")
	if strings.Contains(base, "wpgo") {
		return "wpgo"
	}
	return "pursae" // pursae, pursae-linux-amd64 and any other name
}

// renameUsage rewrites the "wpgo " command prefix in the older commands' usage texts to prog.
func renameUsage(s, prog string) string {
	if prog == "wpgo" {
		return s
	}
	return strings.ReplaceAll(s, "usage: wpgo ", "usage: "+prog+" ")
}

// connectGuarded connects for a command that does not migrate and applies the schema guard
// (internal/db/schema_guard.go) that Migrate applies for serve and migrate: a binary below the
// floor a newer release recorded in the database stops here with the version it needs.
func connectGuarded(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	st, err := db.GuardSchema(ctx, pool, server.Version)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if w := st.Warning(); w != "" {
		log.Printf("warning: %s", w)
	}
	return pool, nil
}

func serve(ctx context.Context, cfg config.Config) error {
	mx := setupMetrics(cfg)
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		pool.Close()
		return err
	}
	if len(applied) > 0 {
		log.Printf("applied migrations %v", applied)
	}
	if err := os.MkdirAll(cfg.UploadsDir, 0o755); err != nil {
		pool.Close()
		return err
	}
	srv, err := server.New(ctx, cfg, pool)
	if err != nil {
		pool.Close()
		return err
	}
	announceSetupToken(ctx, cfg, srv)
	srv.UseMetrics(mx)
	serveMetrics(ctx, cfg, mx, pool)
	httpSrv := newHTTPServer(cfg.Addr, srv.Handler())
	// Shutdown order: stop accepting and drain requests, then stop the scheduler (its jobs finish or
	// go back to the queue), then the background writers, then the database pool.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		log.Printf("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			log.Printf("shutdown: %v; closing the remaining connections", err)
			_ = httpSrv.Close()
		}
	}()
	schedCtx, stopSched := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSched()
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		srv.RunScheduler(schedCtx)
	}()
	go srv.WatchSettings(ctx)    // settings changed by other instances (settings.Store.Watch)
	go srv.WatchCachePurges(ctx) // page cache purges made by other instances
	go confirmUpdate(ctx, cfg.Addr, pool)
	go prunePrevBinary(ctx)
	ln, err := coreupdate.Listen(cfg.Addr) // inherited across a core update's restart
	if err != nil {
		stopSched()
		closePool(pool, 5*time.Second)
		return err
	}
	coreupdate.Serving(ln, httpSrv.Shutdown)
	log.Printf("listening on %s (site %s)", cfg.Addr, cfg.SiteURL)
	serveErr := httpSrv.Serve(ln)
	if coreupdate.Restarting() {
		select {} // the core update execs the new binary; do not exit before it does
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		stopSched()
		closePool(pool, 5*time.Second)
		return serveErr
	}
	<-shutdownDone // Serve returns as soon as Shutdown starts: wait for the requests in flight
	stopSched()
	select {
	case <-schedDone:
	case <-time.After(10 * time.Second):
		log.Printf("shutdown: scheduler still busy; jobs left running go back to the queue")
	}
	bctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	srv.Close(bctx) // background writers (404 log, counters, CDN purges)
	cancel()
	closePool(pool, 5*time.Second)
	log.Printf("stopped")
	return nil
}

// newHTTPServer is the site's http.Server. There is no WriteTimeout and no overall ReadTimeout:
// backup downloads and imports (up to 16 GB), media uploads on slow links, the HeadlessGo
// WebSocket/SSE subscriptions and MCP streams legitimately run for minutes. Slow clients are
// bounded by ReadHeaderTimeout, by the per-read body deadline of server.Handler (a body that
// makes no progress for a minute fails) and by IdleTimeout between keep-alive requests.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// closePool closes the pool, giving up after d: pgxpool.Close waits for every acquired
// connection, and a handler that outlived the shutdown timeout may still hold one.
func closePool(pool interface{ Close() }, d time.Duration) {
	done := make(chan struct{})
	go func() {
		pool.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		log.Printf("shutdown: database connections still in use; exiting anyway")
	}
}

func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// rollbackBlocked says why the site must not go back to the binary of version from ("" when it may):
// the floor this database records (internal/db/schema_guard.go) is above it. Under expand/contract
// a release never raises the floor above the release before it, so this only happens when that rule
// was broken; the site then stays on the new binary rather than restoring one that refuses to start.
func rollbackBlocked(ctx context.Context, pool *pgxpool.Pool, from string) string {
	if pool == nil || from == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	floor, err := db.MinBinary(ctx, pool)
	if err != nil || floor == "" || !coreupdate.Less(from, floor) {
		return ""
	}
	return "the database now needs Pursae " + floor + " or newer, so the site stays on " + server.Version +
		" instead of going back to " + from + " (check /readyz and this log)"
}

// prunePrevBinary removes the binary a core update replaced once it has been kept for
// coreupdate.PrevRetention after the update was confirmed: checked at start and every hour.
func prunePrevBinary(ctx context.Context) {
	exe, err := executable()
	if err != nil {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if coreupdate.PrunePrev(exe, time.Now()) {
			log.Printf("core update: removed the previous binary %s", coreupdate.PrevPath(exe))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// confirmUpdate ends a pending core update once this binary is ready: /readyz answers 200, so the
// database is reachable, every migration of this binary is applied and the schema guard lets it run
// (internal/coreupdate). /readyz is unauthenticated and answered before maintenance mode, Pursae
// Security and the page cache, so neither a hidden login page nor a plugin decides the outcome.
func confirmUpdate(ctx context.Context, addr string, pool *pgxpool.Pool) {
	exe, err := executable()
	if err != nil {
		return
	}
	if st := coreupdate.Read(exe); st == nil || st.Status != "pending" || st.To != server.Version {
		return
	}
	ready, err := healthURL(addr, "/readyz")
	if err != nil {
		return
	}
	if awaitReady(ctx, exe, ready, server.Version, pool, updateConfirmWindow) {
		if err := coreupdate.Restart(exe, 10*time.Second); err != nil {
			log.Printf("core update: restart into the previous binary failed: %v", err)
			if coreupdate.Stopped() {
				os.Exit(1) // the supervisor starts the restored binary
			}
		}
	}
}

// updateConfirmWindow is how long a new binary has to become ready before it is rolled back.
const updateConfirmWindow = 2 * time.Minute

// awaitReady waits up to window for readyURL to answer 200 and confirms the pending update to
// version. When it never does, it puts the previous binary back (coreupdate.Rollback) and reports
// true: the caller then restarts into it. It does not roll back when ctx ends first (shutting down
// is not a failed update) or when the database no longer lets the previous binary run.
func awaitReady(ctx context.Context, exe, readyURL, version string, pool *pgxpool.Pool, window time.Duration) bool {
	wctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	if coreupdate.WaitHealthy(wctx, readyURL) {
		coreupdate.Confirm(exe, version)
		log.Printf("core update to %s confirmed", version)
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	// Running but never ready: go back to the previous binary, unless the database no longer lets
	// that binary run (the schema guard would stop it at once).
	if st := coreupdate.Read(exe); st != nil {
		if why := rollbackBlocked(ctx, pool, st.From); why != "" {
			log.Printf("core update: Pursae %s did not become ready in time; %s", version, why)
			return false
		}
	}
	return coreupdate.Rollback(exe, version)
}
