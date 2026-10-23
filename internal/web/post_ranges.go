package web

type postRange struct {
	clause, order string
	args          []any
}

func timelineRanges(id, before string) []postRange {
	order := "length(" + id + ") DESC," + id + " DESC"
	if before == "" {
		return []postRange{{order: order}}
	}
	// In the equal-length span only order by ID. Keeping the constant length
	// expression in ORDER BY makes SQLite sort all candidates unnecessarily.
	return []postRange{
		{" AND length(" + id + ")=? AND " + id + "<?", id + " DESC", []any{len(before), before}},
		{" AND length(" + id + ")<?", order, []any{len(before)}},
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
