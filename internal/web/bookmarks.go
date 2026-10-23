package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xm/internal/database"
)

func validPostID(id string) bool {
	return len(id) > 0 && len(id) <= 64 && strings.IndexFunc(id, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// PUT is idempotent: repeated saves retain the original bookmark order.
func (a *App) setBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPostID(id) {
		problem(w, 400, "无效的推文 ID")
		return
	}
	var input struct {
		Bookmarked *bool `json:"bookmarked"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Bookmarked == nil {
		problem(w, 400, "请指定收藏状态")
		return
	}
	tx, err := a.store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		problem(w, 500, "无法保存收藏")
		return
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(r.Context(), "SELECT 1 FROM posts WHERE id=?", id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, 404, "推文尚未归档")
		return
	}
	if err != nil {
		problem(w, 500, "无法读取推文")
		return
	}
	if *input.Bookmarked {
		_, err = tx.ExecContext(r.Context(), database.Ignore(a.store.DB.Dialect, "INSERT INTO post_bookmarks(post_id,created_at) VALUES(?,?)", "post_id"), id, time.Now().UnixMicro())
	} else {
		_, err = tx.ExecContext(r.Context(), "DELETE FROM post_bookmarks WHERE post_id=?", id)
	}
	if err != nil {
		problem(w, 500, "无法保存收藏，请重试")
		return
	}
	var at int64
	if err = tx.QueryRowContext(r.Context(), "SELECT COALESCE((SELECT created_at FROM post_bookmarks WHERE post_id=?),0)", id).Scan(&at); err != nil {
		problem(w, 500, "无法读取收藏状态")
		return
	}
	if err = tx.Commit(); err != nil {
		problem(w, 500, "收藏保存未确认，请刷新后重试")
		return
	}
	respond(w, 200, map[string]any{"id": id, "bookmarked": at > 0, "bookmarked_at": at})
}

func parseBookmarkCursor(cursor string) (int64, string, error) {
	parts := strings.Split(cursor, ":")
	if len(parts) != 3 || parts[0] != "b" || !validPostID(parts[2]) {
		return 0, "", errors.New("无效的收藏分页游标")
	}
	at, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || at <= 0 || strconv.FormatInt(at, 10) != parts[1] {
		return 0, "", errors.New("无效的收藏分页游标")
	}
	return at, parts[2], nil
}

// Global bookmarks intentionally ignore monitor visibility. Account bookmarks
// include archived roots and their quoted posts, without changing associations.
func listBookmarkedPosts(ctx context.Context, db *database.DB, target, query, kind string, offset, limit int, before string, count bool) (PostList, error) {
	result := PostList{Items: []PostView{}, Offset: offset, Limit: limit, Total: -1}
	args := []any{}
	from := " FROM post_bookmarks b JOIN posts p ON p.id=b.post_id"
	where := " WHERE 1=1"
	if target != "" {
		// MariaDB cannot correlate the recursive anchor, so start from the
		// account's posts. Other engines walk backwards from each bookmark.
		// UNION terminates cycles and includes quote-only archived posts.
		if db.MariaDB {
			where += ` AND EXISTS (WITH RECURSIVE owners(id) AS (
 SELECT post_id FROM target_posts WHERE target=?
 UNION SELECT q.quote_id FROM owners o JOIN posts q ON q.id=o.id WHERE q.quote_id<>'')
 SELECT 1 FROM owners o WHERE o.id=p.id)`
		} else {
			where += ` AND EXISTS (WITH RECURSIVE owners(id) AS (
 SELECT p.id UNION SELECT parent.id FROM owners o JOIN posts parent ON parent.quote_id=o.id)
 SELECT 1 FROM owners o JOIN target_posts t ON t.post_id=o.id WHERE t.target=?)`
		}
		args = append(args, target)
	}
	filter, filterArgs := postFilterForDB(db, query, kind, false)
	where += filter
	args = append(args, filterArgs...)
	if count {
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*)"+from+where, args...).Scan(&result.Total); err != nil {
			return result, err
		}
	}
	ranges, err := bookmarkRanges(before)
	if err != nil {
		return result, err
	}
	if before != "" {
		offset = 0
	}
	for _, span := range ranges {
		pageArgs := append(append([]any{}, args...), span.args...)
		pageArgs = append(pageArgs, limit+1-len(result.Items), offset)
		rows, err := db.QueryContext(ctx, "SELECT "+postColumns+from+where+span.clause+" ORDER BY "+span.order+" LIMIT ? OFFSET ?", pageArgs...)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			p, e := scanPost(rows)
			if e != nil {
				rows.Close()
				return result, e
			}
			result.Items = append(result.Items, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
		if len(result.Items) > limit {
			break
		}
	}
	result.HasMore = len(result.Items) > limit
	if result.HasMore {
		result.Items = result.Items[:limit]
	}
	if len(result.Items) > 0 {
		last := result.Items[len(result.Items)-1]
		result.NextCursor = fmt.Sprintf("b:%d:%s", last.BookmarkedAt, last.ID)
	}
	if err = attachPosts(ctx, db, result.Items); err != nil {
		return result, err
	}
	return result, nil
}
