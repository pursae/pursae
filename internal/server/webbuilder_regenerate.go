package server

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"wpgo/internal/webbuilder"
)

// Bulk regenerate (Settings › Web Builder › Regenerate pages): only the editor renders a snapshot
// (contract §4, renderStatic runs in the browser), so the screen embeds a frame that loads the
// editor bundle in "regenerate mode" (builder/host/regenerate.ts). The frame walks the pages whose
// published markup came from an older renderer, with GET web-builder/v2/outdated (keyset paging,
// no cap), draws each one and publishes it again through the ordinary document routes, keeping its
// status. A page imported from a WXR file has a document but no snapshot (renderer 0) and takes the
// same path.

// wbOutdatedItem is one page the frame has to publish again.
type wbOutdatedItem struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	Renderer int    `json:"renderer"`
}

const wbOutdatedBatchMax = 100

// wbOutdatedBatch lists up to limit outdated built posts with an id above after, ascending, plus
// the total number that are outdated.
func (s *Server) wbOutdatedBatch(c *gin.Context, after int64, limit int) ([]wbOutdatedItem, int, error) {
	if limit < 1 || limit > wbOutdatedBatchMax {
		limit = wbOutdatedBatchMax
	}
	ctx := c.Request.Context()
	where, args := s.wbOutdatedWhere() // an older renderer, or a changed site style (webbuilder_site_style.go)
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM web_builder_docs d JOIN posts p ON p.id = d.post_id
		WHERE `+where+` AND p.status NOT IN ('trash', 'auto-draft')`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT p.id, p.title, p.type, p.status, d.renderer FROM web_builder_docs d
		JOIN posts p ON p.id = d.post_id
		WHERE `+where+` AND p.status NOT IN ('trash', 'auto-draft') AND p.id > $3
		ORDER BY p.id ASC LIMIT $4`, append(args, after, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []wbOutdatedItem{}
	for rows.Next() {
		var it wbOutdatedItem
		if err := rows.Scan(&it.ID, &it.Title, &it.Type, &it.Status, &it.Renderer); err != nil {
			return nil, 0, err
		}
		if it.Title == "" {
			it.Title = "(no title)"
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

// wbRESTOutdated is GET outdated?after=<post id>&limit=<n>: {renderer, total, items}. Needs
// manage_options, like the settings screen; each publish is then checked by its own route.
func (s *Server) wbRESTOutdated(c *gin.Context, r *restRequest) {
	if wbUser(c, "manage_options") == nil {
		return
	}
	after, _ := strconv.ParseInt(c.Query("after"), 10, 64)
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, total, err := s.wbOutdatedBatch(c, after, limit)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "The pages could not be listed.")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"renderer": webbuilder.RendererVersion, "total": total, "items": items})
}
