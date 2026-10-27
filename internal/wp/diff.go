package wp

import (
	"html"
	"strings"
)

// TextDiff is wp_text_diff() with show_split_view: a side-by-side table of the lines that differ
// between left and right, using WP_Text_Diff_Renderer_Table's markup (table.diff with
// td.diff-deletedline / td.diff-addedline / td.diff-context cells and <del>/<ins> word
// highlights). It returns "" when the two texts are the same, like WordPress.
func TextDiff(left, right string) string {
	left = strings.ReplaceAll(left, "\r\n", "\n")
	right = strings.ReplaceAll(right, "\r\n", "\n")
	if left == right {
		return ""
	}
	a, b := splitLines(left), splitLines(right)
	var sb strings.Builder
	sb.WriteString("<table class=\"diff is-split-view\">\n<tbody>\n")
	var del, ins []string
	flush := func() {
		writeChanged(&sb, del, ins)
		del, ins = nil, nil
	}
	for _, op := range diffLines(a, b) {
		switch op.kind {
		case diffEqual:
			flush()
			cell := "<td class=\"diff-context\"><span class=\"screen-reader-text\">Unchanged: </span>" + html.EscapeString(op.text) + "</td>"
			sb.WriteString("<tr>" + cell + cell + "</tr>\n")
		case diffDelete:
			del = append(del, op.text)
		case diffInsert:
			ins = append(ins, op.text)
		}
	}
	flush()
	sb.WriteString("</tbody>\n</table>")
	return sb.String()
}

// writeChanged prints a block of deleted and added lines. Lines are paired up like the renderer's
// _changed(): a deleted line beside an added one, with the words that differ wrapped in <del>/<ins>
// when the two lines are similar enough; leftover lines print alone with an empty cell beside them.
func writeChanged(sb *strings.Builder, del, ins []string) {
	n := min(len(del), len(ins))
	for i := 0; i < n; i++ {
		l, r := inlineDiff(del[i], ins[i])
		sb.WriteString("<tr><td class=\"diff-deletedline\"><span aria-hidden=\"true\" class=\"dashicons dashicons-minus\"></span><span class=\"screen-reader-text\">Deleted: </span>" + l +
			"</td><td class=\"diff-addedline\"><span aria-hidden=\"true\" class=\"dashicons dashicons-plus\"></span><span class=\"screen-reader-text\">Added: </span>" + r + "</td></tr>\n")
	}
	for _, line := range del[n:] {
		sb.WriteString("<tr><td class=\"diff-deletedline\"><span aria-hidden=\"true\" class=\"dashicons dashicons-minus\"></span><span class=\"screen-reader-text\">Deleted: </span>" +
			html.EscapeString(line) + "</td><td>&nbsp;</td></tr>\n")
	}
	for _, line := range ins[n:] {
		sb.WriteString("<tr><td>&nbsp;</td><td class=\"diff-addedline\"><span aria-hidden=\"true\" class=\"dashicons dashicons-plus\"></span><span class=\"screen-reader-text\">Added: </span>" +
			html.EscapeString(line) + "</td></tr>\n")
	}
}

// inlineDiff returns the two lines escaped, with differing words wrapped in <del> and <ins>.
// When more than the renderer's threshold of the words changed the lines print without marks.
func inlineDiff(left, right string) (string, string) {
	lw, rw := splitWords(left), splitWords(right)
	ops := diffLines(lw, rw)
	changed := 0
	for _, op := range ops {
		if op.kind != diffEqual {
			changed++
		}
	}
	if len(lw)+len(rw) == 0 || float64(changed)/float64(len(lw)+len(rw)) > 0.6 {
		return html.EscapeString(left), html.EscapeString(right)
	}
	var l, r strings.Builder
	for _, op := range ops {
		switch op.kind {
		case diffEqual:
			l.WriteString(html.EscapeString(op.text))
			r.WriteString(html.EscapeString(op.text))
		case diffDelete:
			l.WriteString("<del>" + html.EscapeString(op.text) + "</del>")
		case diffInsert:
			r.WriteString("<ins>" + html.EscapeString(op.text) + "</ins>")
		}
	}
	return l.String(), r.String()
}

type diffKind int

const (
	diffEqual diffKind = iota
	diffDelete
	diffInsert
)

type diffOp struct {
	kind diffKind
	text string
}

// diffLines is a longest-common-subsequence diff of two string slices.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// lcs[i][j] = length of the LCS of a[i:] and b[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{diffEqual, a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{diffDelete, a[i]})
			i++
		default:
			ops = append(ops, diffOp{diffInsert, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{diffDelete, a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{diffInsert, b[j]})
	}
	return ops
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// splitWords splits a line into words and the whitespace between them, keeping both so the
// pieces concatenate back to the line.
func splitWords(s string) []string {
	var out []string
	start := 0
	inSpace := false
	for i, r := range s {
		sp := r == ' ' || r == '\t'
		if i > 0 && sp != inSpace {
			out = append(out, s[start:i])
			start = i
		}
		inSpace = sp
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
