package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/mail"
)

// WP-Cron without visitors: a runner claims due rows of the jobs table with FOR UPDATE SKIP LOCKED,
// so several WordPress Go instances can share one database without running a job twice. Recurring
// events reschedule themselves (wp_schedule_event()); one-off jobs (e-mail) are queued with queueJob.

type jobHandler func(ctx context.Context, payload map[string]any) error

var recurrences = map[string]time.Duration{
	"hourly": time.Hour, "twicedaily": 12 * time.Hour, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour,
}

// coreEvents are the scheduled events a fresh WordPress install has, plus WordPress Go's own.
var coreEvents = []struct{ Name, Recurrence string }{
	{"wp_scheduled_delete", "daily"},            // empty trash older than EMPTY_TRASH_DAYS
	{"wp_scheduled_auto_draft_delete", "daily"}, // drop auto-drafts older than 7 days
	{"delete_expired_transients", "daily"},
	{"wp_session_gc", "hourly"},      // expired login sessions and old login attempts
	{"wpgo_cache_janitor", "hourly"}, // expired page-cache entries
}

// recurrenceInterval maps a recurrence name to its interval: the WordPress names above, or
// "every:<seconds>" for plugin events (schedule_event). Unknown names fall back to a day.
func recurrenceInterval(rec string) time.Duration {
	if d, ok := recurrences[rec]; ok {
		return d
	}
	if n, err := strconv.ParseInt(strings.TrimPrefix(rec, "every:"), 10, 64); err == nil && n >= 60 {
		return time.Duration(n) * time.Second
	}
	return 24 * time.Hour
}

func (s *Server) jobHandlers() map[string]jobHandler {
	return map[string]jobHandler{
		"wp_scheduled_delete": func(ctx context.Context, _ map[string]any) error {
			_, err := s.store.Pool.Exec(ctx, `DELETE FROM posts WHERE status = 'trash' AND updated_at < now() - interval '30 days'`)
			if err == nil {
				_, err = s.store.Pool.Exec(ctx, `DELETE FROM comments WHERE status = 'trash' AND updated_at < now() - interval '30 days'`)
			}
			return err
		},
		"wp_scheduled_auto_draft_delete": func(ctx context.Context, _ map[string]any) error {
			_, err := s.store.Pool.Exec(ctx, `DELETE FROM posts WHERE status = 'auto-draft' AND created_at < now() - interval '7 days'`)
			return err
		},
		"delete_expired_transients": func(ctx context.Context, _ map[string]any) error {
			_, err := s.store.Pool.Exec(ctx, `DELETE FROM transients WHERE expires_at IS NOT NULL AND expires_at < now()`)
			return err
		},
		"wp_session_gc": func(ctx context.Context, _ map[string]any) error {
			_, err := s.store.Pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
			if err == nil {
				_, err = s.store.Pool.Exec(ctx, `DELETE FROM login_attempts WHERE created_at < now() - interval '1 day'`)
			}
			return err
		},
		"wpgo_cache_janitor": func(context.Context, map[string]any) error {
			if c := s.currentCache(); c != nil {
				c.PurgeExpired()
			}
			return nil
		},
		"wp_mail": func(ctx context.Context, p map[string]any) error {
			var msg mail.Message
			raw, _ := json.Marshal(p)
			if err := json.Unmarshal(raw, &msg); err != nil {
				return err
			}
			return s.mailer().Send(ctx, msg)
		},
	}
}

// mailer is wp_mail()'s transport with the site's From address (wordpress@<domain> by default).
func (s *Server) mailer() *mail.Mailer {
	from := s.cfg.MailFrom
	if from == "" {
		host := "localhost"
		if u := s.settings.String("home"); u != "" {
			if h := hostOf(u); h != "" {
				host = h
			}
		}
		from = fmt.Sprintf("%s <wordpress@%s>", s.settings.String("blogname"), host)
	}
	dir := s.cfg.MailDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(filepath.Clean(s.cfg.UploadsDir)), "mail")
	}
	return &mail.Mailer{Host: s.cfg.SMTPHost, Port: s.cfg.SMTPPort, User: s.cfg.SMTPUser, Pass: s.cfg.SMTPPass, From: from, Dir: dir}
}

// queueJob schedules a one-off job (runAt zero means now).
func (s *Server) queueJob(ctx context.Context, name string, payload any, runAt time.Time) error {
	if runAt.IsZero() {
		runAt = time.Now()
	}
	raw, _ := json.Marshal(payload)
	_, err := s.store.Pool.Exec(ctx, `INSERT INTO jobs(name, payload, run_at) VALUES ($1, $2, $3)`, name, raw, runAt)
	return err
}

// sendMail queues an e-mail; delivery happens in the job runner so requests never wait on SMTP.
func (s *Server) sendMail(ctx context.Context, msg mail.Message) {
	if err := s.queueJob(context.WithoutCancel(ctx), "wp_mail", msg, time.Time{}); err != nil {
		slog.Error("queue mail", "err", err)
	}
}

// ensureEvents schedules the recurring core events that are missing.
func (s *Server) ensureEvents(ctx context.Context) {
	for _, e := range coreEvents {
		_, _ = s.store.Pool.Exec(ctx, `INSERT INTO jobs(name, run_at, recurrence)
			SELECT $1, now() + interval '1 minute', $2 WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE name = $1 AND recurrence <> '')`, e.Name, e.Recurrence)
	}
}

// RunScheduler runs scheduled posts and due jobs every 15 seconds.
func (s *Server) RunScheduler(ctx context.Context) {
	s.ensureEvents(ctx)
	worker, _ := os.Hostname()
	worker = fmt.Sprintf("%s:%d", worker, os.Getpid())
	handlers := s.jobHandlers()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		if n, err := s.store.PublishDuePosts(ctx); err != nil {
			if ctx.Err() == nil {
				slog.Error("scheduler: publish_future_post", "err", err)
			}
		} else if n > 0 {
			slog.Info("scheduler: published scheduled posts", "count", n)
			s.purgeAll(ctx)
		}
		s.runDueJobs(ctx, worker, handlers)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) runDueJobs(ctx context.Context, worker string, handlers map[string]jobHandler) {
	// Jobs stuck in "running" (a crashed instance) go back to the queue.
	_, _ = s.store.Pool.Exec(ctx, `UPDATE jobs SET status = 'queued', locked_by = NULL WHERE status = 'running' AND locked_at < now() - interval '10 minutes'`)
	rows, err := s.store.Pool.Query(ctx, `UPDATE jobs SET status = 'running', locked_by = $1, locked_at = now()
		WHERE id IN (SELECT id FROM jobs WHERE status = 'queued' AND run_at <= now() ORDER BY run_at LIMIT 20 FOR UPDATE SKIP LOCKED)
		RETURNING id, name, payload, recurrence, attempts`, worker)
	if err != nil {
		return
	}
	type job struct {
		id        int64
		name, rec string
		payload   map[string]any
		attempts  int
	}
	var due []job
	for rows.Next() {
		var j job
		var raw []byte
		if rows.Scan(&j.id, &j.name, &raw, &j.rec, &j.attempts) == nil {
			_ = json.Unmarshal(raw, &j.payload)
			due = append(due, j)
		}
	}
	rows.Close()
	for _, j := range due {
		h := handlers[j.name]
		var runErr error
		switch {
		case h != nil:
			runErr = h(ctx, j.payload)
		case s.plugins != nil && s.plugins.hooks.HasAction(j.name):
			s.plugins.hooks.DoAction(ctx, j.name, j.payload)
		default:
			runErr = fmt.Errorf("no handler for %q", j.name)
		}
		switch {
		case runErr == nil && j.rec != "":
			_, _ = s.store.Pool.Exec(ctx, `UPDATE jobs SET status = 'queued', attempts = 0, last_error = '', locked_by = NULL,
				run_at = greatest(run_at + $2::interval, now()) WHERE id = $1`, j.id, fmt.Sprintf("%d seconds", int(recurrenceInterval(j.rec).Seconds())))
		case runErr == nil:
			_, _ = s.store.Pool.Exec(ctx, `UPDATE jobs SET status = 'done', locked_by = NULL WHERE id = $1`, j.id)
		case j.attempts+1 < 5:
			// Retry with backoff: 1, 4, 9, 16 minutes.
			_, _ = s.store.Pool.Exec(ctx, `UPDATE jobs SET status = 'queued', attempts = attempts + 1, last_error = $2, locked_by = NULL,
				run_at = now() + make_interval(mins => (attempts + 1) * (attempts + 1)) WHERE id = $1`, j.id, runErr.Error())
		default:
			_, _ = s.store.Pool.Exec(ctx, `UPDATE jobs SET status = 'failed', attempts = attempts + 1, last_error = $2, locked_by = NULL WHERE id = $1`, j.id, runErr.Error())
			slog.Error("job failed", "job", j.name, "err", runErr)
		}
	}
	// Keep the table small: finished one-off jobs are kept for a week for Site Health.
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM jobs WHERE status = 'done' AND recurrence = '' AND updated_at < now() - interval '7 days'`)
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return ""
}
