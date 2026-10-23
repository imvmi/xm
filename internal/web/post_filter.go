package web

// Each EXISTS expands only the current candidate's quote chain. The outer
// timeline can seek its ordered index and stop after LIMIT matches instead of
// materializing the entire archive before every page. UNION terminates cycles.
// Search and media are independent: they may match different quoted posts.
const relatedPostSQL = `WITH RECURSIVE related(id) AS (
 SELECT p.id UNION SELECT q.quote_id FROM related r JOIN posts q ON q.id=r.id WHERE q.quote_id<>'') `

func postFilter(query, kind string, mediaOnly bool) (string, []any) {
	where := ""
	args := []any{}
	if query != "" {
		where += " AND EXISTS (" + relatedPostSQL + `SELECT 1 FROM related r JOIN posts q ON q.id=r.id
 WHERE instr(lower(q.text),lower(?))>0 OR instr(lower(q.username),lower(?))>0)`
		args = append(args, query, query)
	}
	// Keep media correlated to each related ID. A flattened join can make
	// SQLite build a temporary index over every asset of the requested kind.
	media := "EXISTS (" + relatedPostSQL + "SELECT 1 FROM related r WHERE EXISTS (SELECT 1 FROM media a WHERE a.post_id=r.id"
	if mediaOnly && kind != "photo" && kind != "video" && kind != "animated_gif" && kind != "media" {
		where += " AND " + media + "))"
	}
	switch kind {
	case "photo", "animated_gif":
		where += " AND " + media + " AND a.kind=?))"
		args = append(args, kind)
	case "video":
		where += " AND " + media + " AND a.kind IN ('video','animated_gif')))"
	case "media":
		where += " AND " + media + "))"
	case "text":
		where += " AND NOT " + media + "))"
	}
	return where, args
}
