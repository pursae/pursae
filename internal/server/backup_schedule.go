package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/mail"
)

// Scheduled backups: a recurring row of the jobs table (hourly, daily or weekly, like wp_schedule_event())
// whose action, bmgrScheduleJob, the scheduler dispatches through the core hook registry. Each run
// backs up the chosen parts with every user into a "scheduled" archive in the backups folder, then
// deletes the oldest scheduled archives beyond the retention count; manual backups and safety
// snapshots are never pruned. With remote storage set to upload automatically (backup_remote.go)
// the archive is then encrypted and copied to the bucket. A failed run is e-mailed to the admin address.

const (
	bmgrScheduleJob  = "wpgo_backup_scheduled"
	bmgrOptSchedule  = "backup_manager_schedule"        // off | hourly | daily | weekly
	bmgrOptKeep      = "backup_manager_schedule_keep"   // scheduled archives kept
	bmgrOptCats      = "backup_manager_schedule_cats"   // comma-separated category keys
	bmgrOptLast      = "backup_manager_schedule_last"   // {"time", "file", "error"} of the last run
	bmgrOptHour      = "backup_manager_schedule_hour"   // "" (soon after saving) or 0-23, in the site time zone
	bmgrOptNotify    = "backup_manager_schedule_notify" // failure (default) | always | never: e-mail to the admin address
	bmgrKeepDefault  = 7
	bmgrKeepMax      = 100
	bmgrScheduleKind = "scheduled"
)

// bmgrScheduleSettings is the schedule as the screen edits it.
type bmgrScheduleSettings struct {
	Freq   string
	Keep   int
	Cats   []string
	Hour   int    // -1: not chosen
	Notify string // failure | always | never
}

func (s *Server) bmgrSchedule() bmgrScheduleSettings {
	o := bmgrScheduleSettings{Freq: s.settings.String(bmgrOptSchedule), Keep: s.settings.Int(bmgrOptKeep)}
	if o.Freq != "hourly" && o.Freq != "daily" && o.Freq != "weekly" {
		o.Freq = "off"
	}
	o.Hour = -1
	if h, err := strconv.Atoi(strings.TrimSpace(s.settings.String(bmgrOptHour))); err == nil && h >= 0 && h <= 23 {
		o.Hour = h
	}
	o.Notify = s.settings.String(bmgrOptNotify)
	if o.Notify != "always" && o.Notify != "never" {
		o.Notify = "failure"
	}
	if o.Keep < 1 || o.Keep > bmgrKeepMax {
		o.Keep = bmgrKeepDefault
	}
	raw, set := s.settings.Get(bmgrOptCats).(string)
	for _, k := range strings.Split(raw, ",") {
		if bmgrCategoryByKey(strings.TrimSpace(k)) != nil {
			o.Cats = append(o.Cats, strings.TrimSpace(k))
		}
	}
	if !set {
		for _, c := range bmgrCategories {
			o.Cats = append(o.Cats, c.Key)
		}
	}
	return o
}

// bmgrRegisterSchedule hooks the scheduled-backup action into the core hook registry the scheduler
// dispatches unknown job names to. Called once at start-up, after the plugin runtime exists.
func (s *Server) bmgrRegisterSchedule() {
	if s.plugins == nil || s.plugins.hooks == nil {
		return
	}
	s.plugins.hooks.AddAction(bmgrScheduleJob, 10, func(ctx context.Context, _ ...any) {
		if err := s.bmgrScheduledRun(ctx); err != nil {
			slog.Error("scheduled backup", "err", err)
		}
	})
}

// bmgrSyncSchedule makes the jobs table match the settings: one recurring row while the plugin is
// on and a frequency is chosen, none otherwise. An unchanged frequency keeps its next run unless
// resched is set (the time of day changed).
func (s *Server) bmgrSyncSchedule(ctx context.Context, resched bool) error {
	o := s.bmgrSchedule()
	if !s.bmgrOn() || o.Freq == "off" {
		_, err := s.store.Pool.Exec(ctx, `DELETE FROM jobs WHERE name = $1`, bmgrScheduleJob)
		return err
	}
	first := bmgrFirstRunAt(o, time.Now(), s.loc())
	tag, err := s.store.Pool.Exec(ctx, `UPDATE jobs SET recurrence = $2, run_at = $3
		WHERE name = $1 AND recurrence <> '' AND (recurrence <> $2 OR $4)`, bmgrScheduleJob, o.Freq, first, resched)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	_, err = s.store.Pool.Exec(ctx, `INSERT INTO jobs(name, run_at, recurrence)
		SELECT $1, $3, $2 WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE name = $1 AND recurrence <> '')`,
		bmgrScheduleJob, o.Freq, first)
	return err
}

// bmgrFirstRunAt is when a new schedule first runs: soon (ten minutes), so the first archive exists
// the same day, or, when a time of day is chosen, the next time the site clock shows that hour.
// Every later run is a day or a week from there (an hour for the hourly schedule).
func bmgrFirstRunAt(o bmgrScheduleSettings, now time.Time, loc *time.Location) time.Time {
	if o.Hour < 0 || o.Freq == "hourly" {
		return now.Add(10 * time.Minute)
	}
	n := now.In(loc)
	t := time.Date(n.Year(), n.Month(), n.Day(), o.Hour, 0, 0, 0, loc)
	for !t.After(now.Add(time.Minute)) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// bmgrNextRun is when the scheduled backup runs next (zero: not scheduled).
func (s *Server) bmgrNextRun(ctx context.Context) time.Time {
	var t time.Time
	_ = s.store.Pool.QueryRow(ctx, `SELECT run_at FROM jobs WHERE name = $1 AND recurrence <> '' ORDER BY run_at LIMIT 1`, bmgrScheduleJob).Scan(&t)
	return t
}

// bmgrScheduledRun is one scheduled backup: run, record the result, prune old scheduled archives.
func (s *Server) bmgrScheduledRun(ctx context.Context) error {
	o := s.bmgrSchedule()
	if !s.bmgrOn() || o.Freq == "off" {
		return nil
	}
	// The action is reachable through the hook registry, so a second call within the hour is ignored.
	if last, ok := s.settings.Get(bmgrOptLast).(map[string]any); ok {
		if t, err := time.Parse(time.RFC3339, strAny(last["time"])); err == nil && time.Since(t) < time.Hour {
			return nil
		}
	}
	if len(o.Cats) == 0 {
		return s.bmgrRecordRun(ctx, "", errors.New("no parts are selected for scheduled backups"))
	}
	j, err := s.bmgrStart("backup")
	if err != nil {
		return s.bmgrRecordRun(ctx, "", err)
	}
	name, err := s.bmgrRunBackup(ctx, j, bmgrSelection{Categories: o.Cats, UserMode: "all", Kind: bmgrScheduleKind,
		Note: "Scheduled " + o.Freq + " backup"})
	s.bmgrUpdate(j, func(j *bmgrJob) { j.File = name })
	s.bmgrFinish(j, err)
	recErr := err
	if cp := s.bmgrJobByID(j.ID); err == nil && cp != nil && cp.RemoteError != "" {
		// The archive is safe locally; the failed upload is still reported (and e-mailed).
		recErr = fmt.Errorf("the backup %s was written, but the upload to remote storage failed: %s", name, cp.RemoteError)
	}
	rerr := s.bmgrRecordRun(ctx, name, recErr)
	if err != nil {
		return errors.Join(err, rerr)
	}
	s.bmgrPrune(o.Keep)
	return rerr
}

func (s *Server) bmgrRecordRun(ctx context.Context, file string, runErr error) error {
	defer s.bmgrNotify(ctx, file, runErr)
	last := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "file": file, "error": ""}
	if runErr != nil {
		last["error"] = runErr.Error()
	}
	if err := s.settings.Set(ctx, map[string]any{bmgrOptLast: last}); err != nil {
		return err
	}
	return runErr
}

// bmgrNotify e-mails the administrator the outcome of a scheduled run, as the schedule's notify
// setting asks: by default only failures (a full disk, another backup still running).
func (s *Server) bmgrNotify(ctx context.Context, file string, runErr error) {
	mode := s.bmgrSchedule().Notify
	if mode == "never" || (mode == "failure" && runErr == nil) {
		return
	}
	to := strings.TrimSpace(s.settings.String("admin_email"))
	if to == "" {
		return
	}
	name := s.settings.String("blogname")
	subject, body := "["+name+"] Scheduled backup finished", "The scheduled backup finished: "+file+"\n"
	if runErr != nil {
		subject, body = "["+name+"] Scheduled backup FAILED", "The scheduled backup of "+name+" failed:\n\n"+runErr.Error()+"\n\nSee Tools > Backup Manager > Schedule."
	}
	if err := s.mailer().Send(ctx, mail.Message{To: []string{to}, Subject: subject, Body: body}); err != nil {
		slog.Warn("scheduled backup: notification mail", "err", err)
	}
}

// bmgrPrune deletes the oldest scheduled archives so that keep of them remain.
func (s *Server) bmgrPrune(keep int) []string {
	return s.bmgrPruneKind(bmgrScheduleKind, bmgrScheduleKind+"-", keep)
}

// bmgrPruneKind deletes the oldest archives of one kind (manifest kind and file-name prefix) so that
// keep of them remain.
func (s *Server) bmgrPruneKind(kind, prefix string, keep int) []string {
	var sched []bmgrEntry
	for _, e := range s.bmgrList() {
		if e.M != nil && e.M.Kind == kind && strings.HasPrefix(e.Name, prefix) {
			sched = append(sched, e)
		}
	}
	sort.SliceStable(sched, func(i, j int) bool { return sched[i].M.Created.After(sched[j].M.Created) })
	var removed []string
	for i := keep; i < len(sched); i++ {
		if err := os.Remove(filepath.Join(s.bmgrDir(), sched[i].Name)); err == nil {
			removed = append(removed, sched[i].Name)
		}
	}
	return removed
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}

// --- the Schedule tab ---------------------------------------------------------------------------

type bmgrScheduleView struct {
	Freqs             []choice
	Hours             []choice
	Notifies          []choice
	Keep              int
	Cats              []choice
	Next              string
	LastWhen, LastErr string
	LastFile          string
}

func (s *Server) bmgrScheduleView(c *gin.Context) *bmgrScheduleView {
	o := s.bmgrSchedule()
	v := &bmgrScheduleView{Keep: o.Keep}
	for _, f := range [][2]string{{"off", "Off"}, {"hourly", "Hourly"}, {"daily", "Daily"}, {"weekly", "Weekly"}} {
		v.Freqs = append(v.Freqs, choice{Value: f[0], Label: f[1], Checked: f[0] == o.Freq})
	}
	v.Hours = append(v.Hours, choice{Value: "", Label: "Soon after saving", Checked: o.Hour < 0})
	for h := 0; h < 24; h++ {
		v.Hours = append(v.Hours, choice{Value: strconv.Itoa(h), Label: fmt.Sprintf("%02d:00", h), Checked: o.Hour == h})
	}
	for _, f := range [][2]string{{"failure", "Only when a backup fails"}, {"always", "After every backup"}, {"never", "Never"}} {
		v.Notifies = append(v.Notifies, choice{Value: f[0], Label: f[1], Checked: f[0] == o.Notify})
	}
	for _, cat := range bmgrCategories {
		v.Cats = append(v.Cats, choice{Value: cat.Key, Label: cat.Label, Checked: contains(o.Cats, cat.Key)})
	}
	if t := s.bmgrNextRun(c.Request.Context()); !t.IsZero() && o.Freq != "off" {
		v.Next = t.In(s.loc()).Format("2006-01-02 15:04")
	}
	if last, ok := s.settings.Get(bmgrOptLast).(map[string]any); ok {
		if t, err := time.Parse(time.RFC3339, strAny(last["time"])); err == nil {
			v.LastWhen = t.In(s.loc()).Format("2006-01-02 15:04")
		}
		v.LastErr, v.LastFile = strAny(last["error"]), strAny(last["file"])
	}
	return v
}

// bmgrPostSchedule saves the Schedule tab.
func (s *Server) bmgrPostSchedule(c *gin.Context) {
	freq := c.PostForm("schedule")
	if freq != "hourly" && freq != "daily" && freq != "weekly" {
		freq = "off"
	}
	hour := strings.TrimSpace(c.PostForm("hour"))
	if h, err := strconv.Atoi(hour); err != nil || h < 0 || h > 23 {
		hour = ""
	}
	notify := c.PostForm("notify")
	if notify != "always" && notify != "never" {
		notify = "failure"
	}
	oldHour := s.bmgrSchedule().Hour
	keep, err := strconv.Atoi(strings.TrimSpace(c.PostForm("keep")))
	if err != nil || keep < 1 || keep > bmgrKeepMax {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=schedule&bmgr-error="+url.QueryEscape("Keep between 1 and "+strconv.Itoa(bmgrKeepMax)+" scheduled backups.")))
		return
	}
	var cats []string
	for _, k := range c.PostFormArray("cat") {
		if bmgrCategoryByKey(k) != nil && !contains(cats, k) {
			cats = append(cats, k)
		}
	}
	if freq != "off" && len(cats) == 0 {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=schedule&bmgr-error="+url.QueryEscape("Select at least one part to back up.")))
		return
	}
	ctx := c.Request.Context()
	if err := s.settings.Set(ctx, map[string]any{bmgrOptSchedule: freq, bmgrOptKeep: keep, bmgrOptCats: strings.Join(cats, ","),
		bmgrOptHour: hour, bmgrOptNotify: notify}); err != nil {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=schedule&bmgr-error="+url.QueryEscape(err.Error())))
		return
	}
	if err := s.bmgrSyncSchedule(ctx, s.bmgrSchedule().Hour != oldHour); err != nil {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=schedule&bmgr-error="+url.QueryEscape(err.Error())))
		return
	}
	c.Redirect(http.StatusFound, s.bmgrBack("&tab=schedule&bmgr-msg=scheduled"))
}
