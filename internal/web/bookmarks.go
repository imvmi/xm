package web

import (
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
func listBookmarkedPosts(db *database.DB, target, query, kind string, offset, limit int, before string, count bool) (PostList, error) {
	result := PostList{Items: []PostView{}, Offset: offset, Limit: limit, Total: -1}
	ctes := []string{}
	args := []any{}
	from := " FROM post_bookmarks b JOIN posts p ON p.id=b.post_id"
	where := " WHERE 1=1"
	if target != "" {
		ctes = append(ctes, "bookmark_scope(id) AS (SELECT post_id FROM target_posts WHERE target=? UNION SELECT p.quote_id FROM bookmark_scope s JOIN posts p ON p.id=s.id WHERE p.quote_id<>'')")
		args = append(args, target)
		where += " AND EXISTS (SELECT 1 FROM bookmark_scope s WHERE s.id=p.id)"
	}
	if query != "" || kind != "" {
		seed := "SELECT post_id,post_id FROM post_bookmarks"
		if target != "" {
			seed += " WHERE post_id IN (SELECT id FROM bookmark_scope)"
		}
		ctes = append(ctes, "related(root,id) AS ("+seed+" UNION SELECT r.root,p.quote_id FROM related r JOIN posts p ON p.id=r.id WHERE p.quote_id<>'')")
	}
	if query != "" {
		where += " AND EXISTS (SELECT 1 FROM related r JOIN posts q ON q.id=r.id WHERE r.root=p.id AND (instr(lower(q.text),lower(?))>0 OR instr(lower(q.username),lower(?))>0))"
		args = append(args, query, query)
	}
	media := "EXISTS (SELECT 1 FROM related r JOIN media a ON a.post_id=r.id WHERE r.root=p.id"
	switch kind {
	case "photo":
		where += " AND " + media + " AND a.kind='photo')"
	case "video":
		where += " AND " + media + " AND a.kind IN ('video','animated_gif'))"
	case "media":
		where += " AND " + media + ")"
	case "text":
		where += " AND NOT " + media + ")"
	}
	prefix := ""
	if len(ctes) > 0 {
		prefix = "WITH RECURSIVE " + strings.Join(ctes, ",") + " "
	}
	if count {
		if err := db.QueryRow(prefix+"SELECT COUNT(*)"+from+where, args...).Scan(&result.Total); err != nil {
			return result, err
		}
	}
	if before != "" {
		at, id, err := parseBookmarkCursor(before)
		if err != nil {
			return result, err
		}
		where += " AND (b.created_at<? OR (b.created_at=? AND b.post_id<?))"
		args = append(args, at, at, id)
		offset = 0
	}
	args = append(args, limit+1, offset)
	rows, err := db.Query(prefix+"SELECT "+postColumns+from+where+" ORDER BY b.created_at DESC,b.post_id DESC LIMIT ? OFFSET ?", args...)
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
	result.HasMore = len(result.Items) > limit
	if result.HasMore {
		result.Items = result.Items[:limit]
	}
	if len(result.Items) > 0 {
		last := result.Items[len(result.Items)-1]
		result.NextCursor = fmt.Sprintf("b:%d:%s", last.BookmarkedAt, last.ID)
	}
	if err = attachPosts(db, result.Items); err != nil {
		return result, err
	}
	return result, nil
}
