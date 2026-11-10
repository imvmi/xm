package web

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"xm/internal/database"
)

// Follows belong to the stable local archive key, independently of monitoring.
func (a *App) setAccountFollow(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Followed *bool `json:"followed"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Followed == nil {
		problem(w, 400, "请指定关注状态")
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	target := r.PathValue("username")
	tx, err := a.store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		problem(w, 500, "无法保存关注状态")
		return
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(r.Context(), "SELECT 1 FROM monitors WHERE username=? UNION SELECT 1 FROM archive_accounts WHERE target=? LIMIT 1", target, target).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, 404, "账号不存在")
		return
	}
	if err != nil {
		problem(w, 500, "无法读取账号")
		return
	}
	if *input.Followed {
		_, err = tx.ExecContext(r.Context(), database.Ignore(a.store.DB.Dialect, "INSERT INTO account_follows(target,created_at) VALUES(?,?)", "target"), target, time.Now().Unix())
	} else {
		_, err = tx.ExecContext(r.Context(), "DELETE FROM account_follows WHERE target=?", target)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		problem(w, 500, "关注状态保存失败，请重试")
		return
	}
	respond(w, 200, map[string]any{"username": target, "followed": *input.Followed})
}
