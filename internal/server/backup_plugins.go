package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/jackc/pgx/v5"

	"wpgo/internal/plugins"
)

// A restored plugins table and plugin packages come from an archive, not from this site's install and
// consent screens. Packages are verified like an install (trusted signature, compatibility) and a
// dangerous permission (content:html, admin:scripts) stays granted only when this site had granted it
// to the same package before the restore; otherwise it waits on the consent screen.

// bmgrPluginGrant is what this site had granted a plugin before a restore.
type bmgrPluginGrant struct {
	digest  string
	granted []plugins.Permission
}

// bmgrPluginGrants reads the plugins table before a restore replaces it.
func (s *Server) bmgrPluginGrants(ctx context.Context) map[string]bmgrPluginGrant {
	out := map[string]bmgrPluginGrant{}
	rows, err := s.store.Pool.Query(ctx, `SELECT slug, signature, permissions FROM plugins`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var slug, digest string
		var raw []byte
		if rows.Scan(&slug, &digest, &raw) != nil {
			continue
		}
		g := bmgrPluginGrant{digest: digest}
		_ = json.Unmarshal(raw, &g.granted)
		out[slug] = g
	}
	return out
}

// bmgrKeepDangerous reports whether a restored grant of p to slug (package digest) was already given on
// this site.
func bmgrKeepDangerous(before map[string]bmgrPluginGrant, slug, digest string, p plugins.Permission) bool {
	b, ok := before[slug]
	return ok && b.digest != "" && b.digest == digest && slices.Contains(b.granted, p)
}

type bmgrPluginRow struct {
	slug, status, digest string
	granted, missing     []plugins.Permission
}

func bmgrReadPluginRows(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]bmgrPluginRow, error) {
	rows, err := q.Query(ctx, `SELECT slug, status, signature, permissions, missing_permissions FROM plugins ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []bmgrPluginRow
	for rows.Next() {
		var r bmgrPluginRow
		var granted, missing []byte
		if err := rows.Scan(&r.slug, &r.status, &r.digest, &granted, &missing); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(granted, &r.granted)
		_ = json.Unmarshal(missing, &r.missing)
		list = append(list, r)
	}
	return list, rows.Err()
}

// bmgrSplitDangerous moves the dangerous grants this site had not given to the same package into the
// missing list, so the Plugins screen asks for them again. changed reports whether anything moved.
func bmgrSplitDangerous(before map[string]bmgrPluginGrant, r bmgrPluginRow, digest string) (granted, missing []plugins.Permission, changed bool) {
	granted, missing = []plugins.Permission{}, append([]plugins.Permission{}, r.missing...)
	for _, p := range r.granted {
		if p.Dangerous() && !bmgrKeepDangerous(before, r.slug, digest, p) {
			if !slices.Contains(missing, p) {
				missing = append(missing, p)
			}
			changed = true
			continue
		}
		granted = append(granted, p)
	}
	return granted, missing, changed
}

// bmgrRestrictRestoredGrants runs inside the restore transaction, before the restored rows are visible
// to the plugin sync loop.
func bmgrRestrictRestoredGrants(ctx context.Context, tx pgx.Tx, before map[string]bmgrPluginGrant) (int, error) {
	list, err := bmgrReadPluginRows(ctx, tx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range list {
		granted, missing, changed := bmgrSplitDangerous(before, r, r.digest)
		if !changed {
			continue
		}
		g, _ := json.Marshal(granted)
		m, _ := json.Marshal(missing)
		if _, err := tx.Exec(ctx, `UPDATE plugins SET permissions = $2, missing_permissions = $3 WHERE slug = $1`, r.slug, g, m); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// bmgrVetRestoredPlugins checks the packages on disk after the files are in place: a package that does
// not verify with this site's trusted keys, is not compatible or is not the plugin its row names is
// deactivated; the row's digest and signer are set from the package actually installed.
func (s *Server) bmgrVetRestoredPlugins(ctx context.Context, j *bmgrJob, before map[string]bmgrPluginGrant) {
	if s.plugins == nil {
		s.extPluginSystem(ctx) // CLI: the keys without the runtime
	}
	list, err := bmgrReadPluginRows(ctx, s.store.Pool)
	if err != nil {
		s.bmgrLog(j, "warning: checking the restored plugins: %v", err)
		return
	}
	dir, _ := s.pluginDirs()
	for _, r := range list {
		if !plugins.ValidSlug(r.slug) {
			continue
		}
		pkg, err := plugins.LoadPackageFile(filepath.Join(dir, r.slug+".zip"), s.plugins.trusted(), !s.allowUnsignedPlugins())
		if err == nil && pkg.Manifest.Slug != r.slug {
			err = fmt.Errorf("the package is the plugin %q", pkg.Manifest.Slug)
		}
		if err == nil {
			err = pkg.Manifest.CheckCompatible(Version)
		}
		if err == nil && s.builtinPluginSlug(r.slug) {
			err = fmt.Errorf("that slug belongs to a built-in plugin")
		}
		if err != nil {
			reason := "Restored from a backup, but the package could not be verified: " + err.Error()
			_, _ = s.store.Pool.Exec(ctx, `UPDATE plugins SET status = 'inactive', disabled_reason = $2, disabled_at = now() WHERE slug = $1`, r.slug, reason)
			s.bmgrLog(j, "plugin %s deactivated: the restored package could not be verified (%v)", r.slug, err)
			continue
		}
		digest := fmt.Sprintf("%x", pkg.Digest)
		granted, missing, changed := bmgrSplitDangerous(before, r, digest)
		if !changed && digest == r.digest {
			continue
		}
		g, _ := json.Marshal(granted)
		m, _ := json.Marshal(missing)
		_, _ = s.store.Pool.Exec(ctx, `UPDATE plugins SET version = $2, signature = $3, signer = $4, permissions = $5, missing_permissions = $6 WHERE slug = $1`,
			r.slug, pkg.Manifest.Version, digest, pluginSigner(pkg), g, m)
		if changed {
			s.bmgrLog(j, "plugin %s: content:html and admin:scripts must be granted again on the Plugins screen", r.slug)
		}
	}
}
