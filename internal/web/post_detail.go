package web

import (
	"database/sql"
	"errors"
	"net/http"
)

// A quoted post can exist without a target_posts row. Read it by its archive
// ID instead of trying to find it through a paginated account timeline.
func (a *App) postDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("post")
	if !validPostID(id) {
		problem(w, 400, "推文 ID 无效")
		return
	}
	p, err := scanPost(a.store.DB.QueryRowContext(r.Context(), "SELECT "+postColumns+" FROM posts p WHERE p.id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, 404, "推文尚未归档")
		return
	}
	if err != nil {
		problem(w, 500, "读取推文失败")
		return
	}
	result := PostList{Items: []PostView{p}, Total: 1, Limit: 1}
	if err = attachPosts(a.store.DB, result.Items); err != nil {
		problem(w, 500, "读取推文失败")
		return
	}
	a.attachPostAvatars(r, &result)
	respond(w, 200, result)
}
