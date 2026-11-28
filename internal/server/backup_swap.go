package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The file phase of a restore. Every group is first unpacked into a staging folder next to its
// target (same filesystem); bmgrSwapIn then puts the staged groups in place with renames only,
// before the database transaction commits, and keeps what is needed to undo them, so a failure
// on either side leaves the site as it was (no new database with old files, or the other way round).
//
// Plugins and themes are code: after a restore those folders hold exactly the archive's files.
// The whole folder is swapped; whatever the archive does not have (a file planted after a
// compromise, a plugin installed later) is not deleted but moved to <data>/backup-quarantine/<time>/
// outside the web root, where an administrator can look at it. Media is merged: files the archive
// does not have stay where they are (they are users' uploads).

// bmgrReplaceGroups are the file groups a restore replaces as a whole.
var bmgrReplaceGroups = map[string]bool{"plugins": true, "themes": true}

// bmgrSwap records the renames of a restore's file phase.
type bmgrSwap struct {
	undo     []func() error    // run in reverse by rollback
	replaced map[string]string // replaced group -> the folder the previous files were moved to
	roots    map[string]string // replaced group -> its (resolved) folder
	backups  []string          // folders holding media files a restored one replaced
	placed   map[string]int    // group -> files put in place
}

// bmgrStageDir resolves a group's root (a symlinked folder is restored in its target) and
// returns it with a new staging folder beside it.
func bmgrStageDir(root, group string) (resolved, stage string, err error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}
	if resolved, err = filepath.EvalSymlinks(root); err != nil {
		return "", "", err
	}
	stage, err = os.MkdirTemp(filepath.Dir(resolved), ".wpgo-restore-"+group+"-")
	if err != nil {
		return "", "", err
	}
	if fi, err := os.Stat(resolved); err == nil {
		_ = os.Chmod(stage, fi.Mode().Perm()) // MkdirTemp makes 0700; the swapped-in folder keeps the old mode
	}
	return resolved, stage, nil
}

// bmgrSwapIn moves the staged groups into place. On error everything it did is already undone.
func (s *Server) bmgrSwapIn(ctx context.Context, j *bmgrJob, groups []string, staged, roots map[string]string) (*bmgrSwap, error) {
	w := &bmgrSwap{replaced: map[string]string{}, roots: map[string]string{}, placed: map[string]int{}}
	fail := func(err error) (*bmgrSwap, error) {
		if rerr := w.rollback(); rerr != nil {
			return nil, fmt.Errorf("%w (undoing the file changes also failed: %w)", err, rerr)
		}
		return nil, err
	}
	for gi, g := range groups {
		stage, root := staged[g], roots[g]
		if stage == "" {
			continue
		}
		if bmgrReplaceGroups[g] {
			old := filepath.Join(filepath.Dir(root), ".wpgo-restore-old-"+g+"-"+bmgrToken())
			if err := os.Rename(root, old); err != nil {
				return fail(fmt.Errorf("moving the current %s folder aside: %w", g, err))
			}
			if err := os.Rename(stage, root); err != nil {
				if rerr := os.Rename(old, root); rerr != nil {
					err = fmt.Errorf("%w (and putting the old folder back failed: %w; it is at %s)", err, rerr, old)
				}
				return fail(fmt.Errorf("putting the restored %s folder in place: %w", g, err))
			}
			w.undo = append(w.undo, func() error {
				if err := os.Rename(root, stage); err != nil {
					return err
				}
				return os.Rename(old, root)
			})
			w.replaced[g], w.roots[g] = old, root
			_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					w.placed[g]++
				}
				return nil
			})
			continue
		}
		// Merge: each file is renamed into place; one it replaces is moved aside first.
		backup := filepath.Join(filepath.Dir(root), ".wpgo-restore-old-"+g+"-"+bmgrToken())
		w.backups = append(w.backups, backup)
		err := filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, _ := filepath.Rel(stage, p)
			dst, err := bmgrPlaceWithin(root, filepath.Join(root, rel))
			if err != nil {
				return err
			}
			prev := ""
			if fi, err := os.Lstat(dst); err == nil && !fi.IsDir() {
				prev = filepath.Join(backup, rel)
				if err := os.MkdirAll(filepath.Dir(prev), 0o700); err != nil {
					return err
				}
				if err := os.Rename(dst, prev); err != nil {
					return err
				}
			}
			// p is inside the staging folder this restore created; a symlink is moved, not followed.
			if err := os.Rename(p, dst); err != nil { //nolint:gosec // G122: see above
				if prev != "" {
					_ = os.Rename(prev, dst)
				}
				return err
			}
			w.undo = append(w.undo, func() error {
				if prev != "" {
					return os.Rename(prev, dst)
				}
				return os.Remove(dst)
			})
			w.placed[g]++
			if w.placed[g]%25 == 0 {
				s.bmgrProgress(j, "Files: "+g, 60+gi*35/max(len(groups), 1))
			}
			return ctx.Err()
		})
		if err != nil {
			return fail(fmt.Errorf("moving the %s files: %w", g, err))
		}
	}
	return w, nil
}

// rollback undoes the renames, newest first; it goes on after an error and returns the first.
func (w *bmgrSwap) rollback() error {
	var first error
	for i := len(w.undo) - 1; i >= 0; i-- {
		if err := w.undo[i](); err != nil && first == nil {
			first = err
		}
	}
	w.undo = nil
	for _, b := range w.backups {
		os.RemoveAll(b)
	}
	return first
}

// finish runs once the database is committed: the replaced media files are dropped, and of each
// replaced plugins/themes folder the files the archive did not have go to quarantine (the rest
// was superseded by the restored copy). It returns how many files were quarantined and where.
func (w *bmgrSwap) finish(quarantineBase string, now time.Time) (moved int, where string, err error) {
	for _, b := range w.backups {
		os.RemoveAll(b)
	}
	if len(w.replaced) == 0 {
		return 0, "", nil
	}
	dest := filepath.Join(quarantineBase, now.UTC().Format("20060102-150405"))
	var errs []error
	for g, old := range w.replaced {
		n := 0
		_ = filepath.WalkDir(old, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // an unreadable entry stays in the old folder, the walk goes on
			}
			rel, _ := filepath.Rel(old, p)
			if _, err := os.Lstat(filepath.Join(w.roots[g], rel)); err == nil {
				_ = os.Remove(p) //nolint:gosec // G122: p is in the folder this restore moved aside; Remove does not follow a symlink
				return nil
			}
			n++
			return nil
		})
		if n == 0 {
			os.RemoveAll(old)
			continue
		}
		moved += n
		if err := os.MkdirAll(dest, 0o700); err == nil {
			err = os.Rename(old, filepath.Join(dest, g))
			if err == nil {
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w; the files are left in %s", g, err, old))
		} else {
			errs = append(errs, fmt.Errorf("%s: %w; the files are left in %s", g, err, old))
		}
	}
	return moved, dest, errors.Join(errs...)
}

// bmgrQuarantineDir is where restores move plugin and theme files the archive did not have:
// beside the uploads, plugins and themes folders, outside the web root.
func (s *Server) bmgrQuarantineDir() string {
	return filepath.Join(filepath.Dir(filepath.Clean(s.cfg.UploadsDir)), "backup-quarantine")
}
