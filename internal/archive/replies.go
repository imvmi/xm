package archive

import (
	"encoding/json"
	"strings"
	"xm/internal/database"
)

// ReplyAuthorID reads the stable user ID from the saved reply itself; usernames
// and the first @mention cannot reliably identify its parent after a rename.
func ReplyAuthorID(raw []byte) string {
	var v object
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	if wrapped := at(v, "tweet"); wrapped != nil {
		return str(at(wrapped, "legacy", "in_reply_to_user_id_str"))
	}
	return str(at(v, "legacy", "in_reply_to_user_id_str"))
}

// NULL is an unprocessed legacy row; an empty string is a processed unknown.
// Bounded batches make this resumable without reparsing all raw JSON on startup.
func ensureReplyMetadata(db *database.DB) error {
	rows, err := db.Query("SELECT * FROM posts WHERE 1=0")
	if err != nil {
		return err
	}
	columns, err := rows.Columns()
	rows.Close()
	if err != nil {
		return err
	}
	found := false
	for _, column := range columns {
		if column == "reply_to_author_id" {
			found = true
		}
	}
	if !found {
		if _, err = db.Exec("ALTER TABLE posts ADD COLUMN reply_to_author_id VARCHAR(64) DEFAULT NULL"); err != nil {
			return err
		}
	}
	if _, err = db.Exec("UPDATE posts SET reply_to_author_id='' WHERE reply_to='' AND reply_to_author_id IS NULL"); err != nil {
		return err
	}
	after := ""
	for {
		rows, err = db.Query("SELECT p.id,p.raw_json,COALESCE(parent.author_id,'') FROM posts p LEFT JOIN posts parent ON parent.id=p.reply_to WHERE p.reply_to_author_id IS NULL AND p.id>? ORDER BY p.id LIMIT 250", after)
		if err != nil {
			return err
		}
		type entry struct{ id, raw, parent string }
		batch := []entry{}
		for rows.Next() {
			var e entry
			if err = rows.Scan(&e.id, &e.raw, &e.parent); err != nil {
				break
			}
			batch = append(batch, e)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		after = batch[len(batch)-1].id
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, e := range batch {
			author := ReplyAuthorID([]byte(e.raw))
			if author == "" {
				author = e.parent
			}
			if _, err = tx.Exec("UPDATE posts SET reply_to_author_id=? WHERE id=? AND reply_to_author_id IS NULL", strings.TrimSpace(author), e.id); err != nil {
				break
			}
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
}
