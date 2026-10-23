package web

import (
	"strings"
	"xm/internal/database"
)

type postRange struct {
	clause, order string
	args          []any
}

// Only internal, qualified posts/target_posts IDs are passed here.
func timelineIDLength(db *database.DB, id string) string {
	if db.MariaDB {
		alias, _, _ := strings.Cut(id, ".")
		return alias + ".xm_id_length"
	}
	return "length(" + id + ")"
}

func timelineRanges(db *database.DB, id, before string) []postRange {
	length := timelineIDLength(db, id)
	order := length + " DESC," + id + " DESC"
	if before == "" {
		return []postRange{{order: order}}
	}
	// In the equal-length span only order by ID. Keeping the constant length
	// expression in ORDER BY makes SQLite sort all candidates unnecessarily.
	return []postRange{
		{" AND " + length + "=? AND " + id + "<?", id + " DESC", []any{len(before), before}},
		{" AND " + length + "<?", order, []any{len(before)}},
	}
}

func bookmarkRanges(before string) ([]postRange, error) {
	order := "b.created_at DESC,b.post_id DESC"
	if before == "" {
		return []postRange{{order: order}}, nil
	}
	at, id, err := parseBookmarkCursor(before)
	if err != nil {
		return nil, err
	}
	// Two index seeks also avoid rescanning newer bookmarks on deep pages.
	return []postRange{
		{" AND b.created_at=? AND b.post_id<?", "b.post_id DESC", []any{at, id}},
		{" AND b.created_at<?", order, []any{at}},
	}, nil
}

func timelineSource(db *database.DB, target string, page bool) (from, where, id string, args []any) {
	if target != "" {
		return " FROM target_posts t JOIN posts p ON p.id=t.post_id", " WHERE t.target=?", "t.post_id", []any{target}
	}
	// Visibility applies only to roots on the home feed. Scalar membership
	// keeps MariaDB from turning EXISTS into a semijoin that sorts all roots.
	visible := "SELECT 1 FROM target_posts t LEFT JOIN monitors m ON m.username=t.target LEFT JOIN account_preferences ap ON ap.target=t.target WHERE t.post_id=p.id AND COALESCE(m.show_in_timeline,ap.show_in_timeline,1)=1"
	where = " WHERE EXISTS (" + visible + ")"
	if db.MariaDB && page {
		where = " WHERE (" + visible + " LIMIT 1)=1"
	}
	return " FROM posts p", where + " AND " + rootPostSQL, "p.id", nil
}

func timelinePageSource(db *database.DB, target string) string {
	if db.MariaDB {
		// Drive LIMIT/cursor pages from the ordering index even when estimates
		// prefer a selective join followed by a full sort. Counts remain free to
		// choose their own plan. STRAIGHT_JOIN keeps account membership first.
		if target != "" {
			return " FROM target_posts t FORCE INDEX (target_posts_timeline) STRAIGHT_JOIN posts p ON p.id=t.post_id"
		}
		return " FROM posts p FORCE INDEX (posts_timeline)"
	}
	from, _, _, _ := timelineSource(db, target, false)
	return from
}
