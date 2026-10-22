package web

import (
	"database/sql"
	"errors"
	"net/http"

	"xm/internal/database"
)

// Summary cards intentionally contain no nested quotes, media, or archive links.
type ReplySummary struct {
	ID         string `json:"id"`
	AuthorID   string `json:"-"`
	AvatarURL  string `json:"avatar_url,omitempty"`
	HasReplies bool   `json:"has_replies,omitempty"`
	Username   string `json:"username"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	Text       string `json:"text"`
	ReplyTo    string `json:"reply_to,omitempty"`
	SelfReply  bool   `json:"self_reply"`
}

func replySummary(p PostView, limit int) ReplySummary {
	text := p.Text
	if limit > 0 {
		runes := []rune(text)
		if len(runes) > limit {
			text = string(runes[:limit]) + "…"
		}
	}
	return ReplySummary{ID: p.ID, AuthorID: p.AuthorID, AvatarURL: p.AvatarURL, HasReplies: p.HasReplies, Username: p.Username, Name: p.Name, CreatedAt: p.CreatedAt, Text: text, ReplyTo: p.ReplyTo, SelfReply: p.SelfReply}
}

func attachReplyPreviews(db *database.DB, items []PostView) error {
	ids := []string{}
	seen := map[string]bool{}
	var visit func([]PostView)
	visit = func(posts []PostView) {
		for _, p := range posts {
			if p.ReplyTo != "" && !seen[p.ReplyTo] {
				ids = append(ids, p.ReplyTo)
				seen[p.ReplyTo] = true
			}
			if p.Quote != nil {
				visit([]PostView{*p.Quote})
			}
		}
	}
	visit(items)
	parents := map[string]PostView{}
	for start := 0; start < len(ids); start += 500 {
		args, marks := postBatchArgs(ids[start:min(start+500, len(ids))])
		rows, err := db.Query("SELECT "+postColumns+" FROM posts p WHERE p.id IN ("+marks+")", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			p, scanErr := scanPost(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			parents[p.ID] = p
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	var attach func(*PostView)
	attach = func(p *PostView) {
		if parent, ok := parents[p.ReplyTo]; ok && parent.ID != p.ID {
			preview := replySummary(parent, 160)
			p.ReplyPreview = &preview
		}
		if p.Quote != nil {
			attach(p.Quote)
		}
	}
	for i := range items {
		attach(&items[i])
	}
	return nil
}

type ReplyContext struct {
	Items      []PostView `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
	End        string     `json:"end"`
	MissingID  string     `json:"missing_id,omitempty"`
}

// Fetch only archived ancestors, in bounded pages. This endpoint is protected
// by the admin session middleware and never performs a request to X.
func (a *App) replyContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("post")
	if !validPostID(id) {
		problem(w, 400, "推文 ID 无效")
		return
	}
	result := ReplyContext{Items: []PostView{}, End: "beginning"}
	seen := map[string]bool{}
	for len(result.Items) < 20 {
		if seen[id] {
			result.End = "cycle"
			break
		}
		seen[id] = true
		p, err := scanPost(a.store.DB.QueryRowContext(r.Context(), "SELECT "+postColumns+" FROM posts p WHERE p.id=?", id))
		if errors.Is(err, sql.ErrNoRows) {
			if len(result.Items) == 0 {
				problem(w, 404, "推文尚未归档")
				return
			}
			result.End = "missing"
			result.MissingID = id
			break
		}
		if err != nil {
			problem(w, 500, "读取上下文失败")
			return
		}
		result.Items = append(result.Items, p)
		if p.ReplyTo == "" {
			break
		}
		id = p.ReplyTo
		if len(result.Items) == 20 {
			result.End = "more"
			result.NextCursor = id
		}
	}
	for left, right := 0, len(result.Items)-1; left < right; left, right = left+1, right-1 {
		result.Items[left], result.Items[right] = result.Items[right], result.Items[left]
	}
	if err := attachPosts(a.store.DB, result.Items); err != nil {
		problem(w, 500, "读取上下文失败")
		return
	}
	list := PostList{Items: result.Items}
	a.attachPostAvatars(r, &list)
	result.Items = list.Items
	respond(w, 200, result)
}

// Replies contains only immediate children; siblings are never presented as a
// single chain. Further levels are fetched explicitly, keeping each page bounded.
type ReplyList struct {
	Items      []PostView `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
	HasMore    bool       `json:"has_more"`
}

func (a *App) postReplies(w http.ResponseWriter, r *http.Request) {
	id, after := r.PathValue("post"), r.URL.Query().Get("after")
	if !validPostID(id) || (after != "" && !validPostID(after)) {
		problem(w, 400, "推文 ID 或分页游标无效")
		return
	}
	var exists int
	err := a.store.DB.QueryRowContext(r.Context(), "SELECT 1 FROM posts WHERE id=?", id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, 404, "推文尚未归档")
		return
	}
	if err != nil {
		problem(w, 500, "读取回复失败")
		return
	}
	query := "SELECT " + postColumns + " FROM posts p WHERE p.reply_to=? AND p.id<>?"
	args := []any{id, id}
	if after != "" {
		query += " AND (length(p.id)>? OR (length(p.id)=? AND p.id>?))"
		args = append(args, len(after), len(after), after)
	}
	rows, err := a.store.DB.QueryContext(r.Context(), query+" ORDER BY length(p.id) ASC,p.id ASC LIMIT 21", args...)
	if err != nil {
		problem(w, 500, "读取回复失败")
		return
	}
	result := ReplyList{Items: []PostView{}}
	for rows.Next() {
		p, scanErr := scanPost(rows)
		if scanErr != nil {
			rows.Close()
			problem(w, 500, "读取回复失败")
			return
		}
		result.Items = append(result.Items, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		problem(w, 500, "读取回复失败")
		return
	}
	if len(result.Items) > 20 {
		result.Items = result.Items[:20]
		result.HasMore = true
		result.NextCursor = result.Items[19].ID
	}
	if err := attachPosts(a.store.DB, result.Items); err != nil {
		problem(w, 500, "读取回复失败")
		return
	}
	list := PostList{Items: result.Items}
	a.attachPostAvatars(r, &list)
	result.Items = list.Items
	respond(w, 200, result)
}
