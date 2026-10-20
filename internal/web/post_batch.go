package web

import (
	"strings"

	"xm/internal/archive"
	"xm/internal/database"
)

// Fetch each quote level and its media in batches. A page costs a bounded
// number of queries even when it contains hundreds of posts and shared quotes.
func attachPosts(db *database.DB, items []PostView) error {
	posts := make(map[string]PostView)
	frontier := items
	for _, p := range items {
		posts[p.ID] = p
	}
	for depth := 0; depth < archive.MaxQuoteDepth && len(frontier) > 0; depth++ {
		ids := []string{}
		requested := map[string]bool{}
		for _, p := range frontier {
			if _, exists := posts[p.QuoteID]; p.QuoteID != "" && !exists && !requested[p.QuoteID] {
				ids = append(ids, p.QuoteID)
				requested[p.QuoteID] = true
			}
		}
		frontier = nil
		for start := 0; start < len(ids); start += 500 {
			args, marks := postBatchArgs(ids[start:min(start+500, len(ids))])
			rows, err := db.Query("SELECT "+postColumns+" FROM posts p WHERE p.id IN ("+marks+")", args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				p, err := scanPost(rows)
				if err != nil {
					rows.Close()
					return err
				}
				posts[p.ID] = p
				frontier = append(frontier, p)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
	}
	ids := make([]string, 0, len(posts))
	for id := range posts {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += 500 {
		args, marks := postBatchArgs(ids[start:min(start+500, len(ids))])
		rows, err := db.Query("SELECT post_id,media_id,kind,status,last_error,bytes,sha256 FROM media WHERE post_id IN ("+marks+") ORDER BY post_id,media_id", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var m MediaView
			if err = rows.Scan(&id, &m.ID, &m.Kind, &m.Status, &m.Error, &m.Bytes, &m.Version); err != nil {
				rows.Close()
				return err
			}
			m.Version = mediaVersion(m.Version)
			p := posts[id]
			p.Media = append(p.Media, m)
			posts[id] = p
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	var attach func(string, int, map[string]bool) PostView
	attach = func(id string, depth int, seen map[string]bool) PostView {
		p := posts[id]
		seen[id] = true
		defer delete(seen, id)
		if p.QuoteID != "" {
			p.QuoteUnavailable = true
			if _, exists := posts[p.QuoteID]; exists && depth < archive.MaxQuoteDepth && !seen[p.QuoteID] {
				q := attach(p.QuoteID, depth+1, seen)
				p.Quote, p.QuoteUnavailable = &q, false
			}
		}
		return p
	}
	for i := range items {
		items[i] = attach(items[i].ID, 0, map[string]bool{})
	}
	return nil
}

func postBatchArgs(ids []string) ([]any, string) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args, strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
}
