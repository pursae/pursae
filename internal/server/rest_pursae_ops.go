package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Site operations of pursae/v1 for AI tools and hosting automation: delete a theme, health summary,
// updates (core, plugins, themes) and backups. Each reuses the engine behind the matching admin screen.

func (s *Server) registerPursaeOps(guard func(string, restHandler) restHandler) {
	api := s.rest
	api.register(pursaeNS, `/pursae/v1/themes/(?P<slug>[a-z0-9_\-]+)`, map[string]restHandler{
		http.MethodDelete: guard("delete_themes", s.pursaeThemeDelete)}, nil)
	api.register(pursaeNS, "/pursae/v1/health", map[string]restHandler{http.MethodGet: guard("manage_options", s.pursaeHealth)}, nil)
	api.register(pursaeNS, "/pursae/v1/updates", map[string]restHandler{
		http.MethodGet:  guard("update_plugins", s.pursaeUpdatesList),
		http.MethodPost: guard("update_plugins", s.pursaeUpdatesRun),
	}, nil)
	api.register(pursaeNS, "/pursae/v1/backups", map[string]restHandler{
		http.MethodGet:  guard("manage_options", s.pursaeBackupsList),
		http.MethodPost: guard("manage_options", s.pursaeBackupCreate),
	}, nil)
	api.register(pursaeNS, "/pursae/v1/backups/remote", map[string]restHandler{
		http.MethodGet:  guard("manage_options", s.pursaeBackupRemoteList),
		http.MethodPost: guard("manage_options", s.pursaeBackupRemoteUpload),
	}, nil)
}

// pursaeThemeDelete is DELETE /pursae/v1/themes/{slug}: an inactive theme goes (deleteTheme).
func (s *Server) pursaeThemeDelete(c *gin.Context, r *restRequest) {
	slug := r.Params["slug"]
	t := s.installedTheme(slug)
	if t == nil {
		restErr(c, http.StatusNotFound, "rest_theme_not_found", "Theme not found.")
		return
	}
	prev := s.themeJSON(t)
	if err := s.deleteTheme(c.Request.Context(), slug); err != nil {
		restErr(c, http.StatusBadRequest, "rest_theme_delete_failed", err.Error())
		return
	}
	restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
}

// pursaeHealth is GET /pursae/v1/health: version, database, job queue, storage, plugins, cache, the
// score of the Site Health screen and which updates wait.
func (s *Server) pursaeHealth(c *gin.Context, _ *restRequest) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	out := map[string]any{"version": Version, "go": runtime.Version(), "time_gmt": time.Now().UTC().Format("2006-01-02T15:04:05")}
	dbInfo := map[string]any{"ok": false}
	var pg string
	if err := s.store.Pool.QueryRow(ctx, `SHOW server_version`).Scan(&pg); err == nil {
		dbInfo["ok"], dbInfo["version"] = true, pg
	} else {
		dbInfo["error"] = err.Error()
	}
	out["database"] = dbInfo
	jobs := map[string]any{}
	var queued, running, failed, overdue int
	var lastErr string
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'queued'), count(*) FILTER (WHERE status = 'running'),
		count(*) FILTER (WHERE status = 'failed'), count(*) FILTER (WHERE status = 'queued' AND run_at < now() - interval '15 minutes')
		FROM jobs`).Scan(&queued, &running, &failed, &overdue); err == nil {
		jobs["queued"], jobs["running"], jobs["failed"], jobs["overdue"] = queued, running, failed, overdue
	}
	_ = s.store.Pool.QueryRow(ctx, `SELECT name || ': ' || last_error FROM jobs WHERE last_error <> '' ORDER BY updated_at DESC LIMIT 1`).Scan(&lastErr)
	jobs["last_error"] = lastErr
	out["cron"] = jobs
	h := s.siteHealthSummary(ctx)
	out["site_health"] = map[string]any{"good": h.Good, "recommended": h.Recommended, "critical": h.Critical, "status": h.Label}
	var activePlugins, inactivePlugins int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'active'), count(*) FILTER (WHERE status <> 'active') FROM plugins`).Scan(&activePlugins, &inactivePlugins)
	out["plugins"] = map[string]any{"active": activePlugins, "inactive": inactivePlugins, "runtime_ok": s.plugins != nil && s.plugins.initErr == nil}
	out["cache_enabled"] = s.settings.Bool("wpgo_cache_enabled")
	out["theme"] = s.activeTheme().Slug
	updates := s.pursaeUpdateSummary(c)
	out["updates_available"] = len(updates)
	if bk := s.bmgrList(); len(bk) > 0 {
		out["last_backup"] = map[string]any{"file": bk[0].Name, "date_gmt": bk[0].Modified.UTC().Format("2006-01-02T15:04:05"), "size": bk[0].Size}
	}
	restJSON(c, http.StatusOK, out)
}

// pursaeUpdateSummary lists the updates the Manager offers: core, plugins and themes.
func (s *Server) pursaeUpdateSummary(c *gin.Context) []map[string]any {
	list := []map[string]any{}
	if v := s.coreUpdateAvailable(); v != "" {
		list = append(list, map[string]any{"type": "core", "slug": "core", "installed": Version, "available": v})
	}
	for _, r := range s.pluginsWithUpdates(c, s.installedPlugins(c)) {
		if r.Update != nil {
			list = append(list, map[string]any{"type": "plugin", "slug": r.Slug, "installed": r.Version, "available": r.Update.Version})
		}
	}
	themes := s.hubThemeUpdates(c.Request.Context())
	slugs := make([]string, 0, len(themes))
	for slug := range themes {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		inst := ""
		if t := s.installedTheme(slug); t != nil {
			inst = t.Version
		}
		list = append(list, map[string]any{"type": "theme", "slug": slug, "installed": inst, "available": themes[slug].Version})
	}
	return list
}

func (s *Server) pursaeUpdatesList(c *gin.Context, _ *restRequest) {
	restJSON(c, http.StatusOK, s.pursaeUpdateSummary(c))
}

// pursaeUpdatesRun is POST /pursae/v1/updates: {"type": "plugin"|"theme"|"core", "slug": "..."}.
// Plugins and themes are updated from Pursae Manager like the Updates screen does it; "core" replaces
// the binary and restarts the site a second after the answer.
func (s *Server) pursaeUpdatesRun(c *gin.Context, r *restRequest) {
	typ, _ := r.Body["type"].(string)
	slug, _ := r.Body["slug"].(string)
	u := restCurrent(c)
	ctx := c.Request.Context()
	switch typ {
	case "plugin":
		m, err := s.hubUpdatePlugin(withExtActor(ctx, u), slug)
		if errors.Is(err, errPluginReloadDenied) {
			restErr(c, http.StatusForbidden, "rest_cannot_activate_plugin", err.Error())
			return
		}
		if err != nil {
			restErr(c, http.StatusBadRequest, "rest_update_failed", err.Error())
			return
		}
		restJSON(c, http.StatusOK, map[string]any{"type": typ, "slug": slug, "updated": true, "version": m.Version})
	case "theme":
		if !u.Can("update_themes", nil) {
			restErr(c, http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to update themes for this site.")
			return
		}
		t, err := s.hubUpdateTheme(c, slug)
		if err != nil {
			restErr(c, http.StatusBadRequest, "rest_update_failed", err.Error())
			return
		}
		restJSON(c, http.StatusOK, map[string]any{"type": typ, "slug": slug, "updated": true, "version": t.Version})
	case "core":
		if !u.Can("update_core", nil) {
			restErr(c, http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to update this site.")
			return
		}
		if !coreBusy.CompareAndSwap(false, true) {
			restErr(c, http.StatusConflict, "rest_update_busy", errCoreBusy.Error())
			return
		}
		handedOver := false
		defer func() {
			if !handedOver {
				coreBusy.Store(false)
			}
		}()
		target, err := coreUpdateTarget()
		if err != nil {
			restErr(c, http.StatusBadRequest, "rest_update_failed", err.Error())
			return
		}
		version, err := s.coreUpdate(ctx, target)
		if err != nil {
			restErr(c, http.StatusBadRequest, "rest_update_failed", err.Error())
			return
		}
		slog.Info("core updated through the REST API, restarting", "from", Version, "to", version)
		s.hubEvent("core_update", "core", "", version)
		handedOver = true
		restJSON(c, http.StatusOK, map[string]any{"type": typ, "updated": true, "version": version, "restarting": true})
		c.Writer.Flush()
		s.coreRestartLater(target)
	default:
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "type must be plugin, theme or core.")
	}
}

// pursaeBackupsList is GET /pursae/v1/backups: the stored archives, or with ?job=<id> a running one.
func (s *Server) pursaeBackupsList(c *gin.Context, _ *restRequest) {
	if id := c.Query("job"); id != "" {
		j := s.bmgrJobByID(id)
		if j == nil {
			restErr(c, http.StatusNotFound, "rest_backup_job_not_found", "Unknown backup job.")
			return
		}
		restJSON(c, http.StatusOK, map[string]any{"job": j.ID, "kind": j.Kind, "state": j.State, "phase": j.Phase, "percent": j.Percent, "error": j.Error,
			"file": j.File, "remote": j.Remote, "remote_error": j.RemoteError})
		return
	}
	out := []map[string]any{}
	for _, e := range s.bmgrList() {
		row := map[string]any{"file": e.Name, "size": e.Size, "date_gmt": e.Modified.UTC().Format("2006-01-02T15:04:05"), "valid": e.Err == ""}
		if e.M != nil {
			row["note"] = e.M.Note
		}
		out = append(out, row)
	}
	restJSON(c, http.StatusOK, out)
}

// pursaeBackupCreate is POST /pursae/v1/backups: starts a backup in the background and answers with
// the job to poll. Body: {"categories": [...], "note": "..."}; without categories everything is backed up.
func (s *Server) pursaeBackupCreate(c *gin.Context, r *restRequest) {
	sel := bmgrSelection{UserMode: "all"}
	if v, ok := r.Body["categories"].([]any); ok {
		for _, x := range v {
			if k, ok := x.(string); ok {
				sel.Categories = append(sel.Categories, k)
			}
		}
	}
	if len(sel.Categories) == 0 {
		for _, cat := range bmgrCategories {
			sel.Categories = append(sel.Categories, cat.Key)
		}
	}
	valid := map[string]bool{}
	for _, cat := range bmgrCategories {
		valid[cat.Key] = true
	}
	for _, k := range sel.Categories {
		if !valid[k] {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Unknown backup category: "+k)
			return
		}
	}
	if n, _ := r.Body["note"].(string); n != "" {
		sel.Note = strings.TrimSpace(n)
		if len(sel.Note) > 300 {
			sel.Note = sel.Note[:300]
		}
	}
	j, err := s.bmgrStart("backup")
	if err != nil {
		restErr(c, http.StatusConflict, "rest_backup_busy", err.Error())
		return
	}
	safeGo("rest_pursae_ops.pursaeBackupCreate", func() {
		name, err := s.bmgrRunBackup(context.Background(), j, sel)
		s.bmgrUpdate(j, func(j *bmgrJob) { j.File = name })
		s.bmgrFinish(j, err)
	})
	restJSON(c, http.StatusAccepted, map[string]any{"job": j.ID, "state": "running"})
}

// pursaeBackupRemoteList is GET /pursae/v1/backups/remote: the remote storage settings without their
// secrets (only whether they are set) and the encrypted backups in the bucket.
func (s *Server) pursaeBackupRemoteList(c *gin.Context, _ *restRequest) {
	st := s.bmgrRemote()
	out := map[string]any{"configured": st.configured(), "endpoint": st.Endpoint, "region": st.Region, "bucket": st.Bucket,
		"prefix": st.Prefix, "path_style": st.PathStyle, "auto": st.Auto, "keep": st.Keep, "access_key_set": st.AccessKey != "",
		"secret_key_set": st.SecretKey != "", "passphrase_set": st.Passphrase != "", "backups": []bmgrRemoteObject{}}
	if !st.configured() {
		restJSON(c, http.StatusOK, out)
		return
	}
	if err := st.problem(); err != nil {
		out["error"] = err.Error()
		restJSON(c, http.StatusOK, out)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	list, err := bmgrRemoteList(ctx, bmgrRemoteClient(st), st)
	if err != nil {
		out["error"] = err.Error()
	} else if len(list) > 0 {
		out["backups"] = list
	}
	restJSON(c, http.StatusOK, out)
}

// pursaeBackupRemoteUpload is POST /pursae/v1/backups/remote {"file": "<archive>"}: encrypts and
// uploads a stored archive in the background; poll GET /pursae/v1/backups?job=<id>.
func (s *Server) pursaeBackupRemoteUpload(c *gin.Context, r *restRequest) {
	file, _ := r.Body["file"].(string)
	if err := s.bmgrRemote().problem(); err != nil {
		restErr(c, http.StatusBadRequest, "rest_backup_remote_not_configured", err.Error())
		return
	}
	if s.bmgrPath(file) == "" {
		restErr(c, http.StatusNotFound, "rest_backup_not_found", "Backup not found.")
		return
	}
	j, err := s.bmgrStart("upload")
	if err != nil {
		restErr(c, http.StatusConflict, "rest_backup_busy", err.Error())
		return
	}
	safeGo("rest_pursae_ops.pursaeBackupRemoteUpload", func() {
		s.bmgrUpdate(j, func(j *bmgrJob) { j.File = file })
		key, err := s.bmgrRemoteUpload(s.bmgrCancelable(j), file, func(f string, a ...any) { s.bmgrLog(j, f, a...) },
			func(p int) { s.bmgrProgress(j, "Uploading to remote storage", p) })
		s.bmgrUpdate(j, func(j *bmgrJob) { j.Remote = key })
		s.bmgrFinish(j, err)
	})
	restJSON(c, http.StatusAccepted, map[string]any{"job": j.ID, "state": "running"})
}
