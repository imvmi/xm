package web

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"xm/internal/archive"
)

type runErrorRecord struct {
	ID         int64  `json:"id"`
	Target     string `json:"target"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

func (a *App) readErrorRun(w http.ResponseWriter, r *http.Request) (*runErrorRecord, bool) {
	id, err := strconv.ParseInt(r.PathValue("run"), 10, 64)
	if err != nil || id <= 0 {
		problem(w, 400, "任务 ID 无效")
		return nil, false
	}
	run := &runErrorRecord{ID: id}
	err = a.store.DB.QueryRowContext(r.Context(), "SELECT target,status,message,started_at,COALESCE(finished_at,'') FROM runs WHERE id=?", id).Scan(&run.Target, &run.Status, &run.Message, &run.StartedAt, &run.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, 404, "任务记录不存在或已清理")
		return nil, false
	}
	if err != nil {
		problem(w, 500, "读取任务详情失败")
		return nil, false
	}
	return run, true
}

func (a *App) runErrors(w http.ResponseWriter, r *http.Request) {
	run, ok := a.readErrorRun(w, r)
	if !ok {
		return
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		var err error
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 || offset > 10000000 {
			problem(w, 400, "分页位置无效")
			return
		}
	}
	const limit = 30
	items, total, err := a.store.MediaProblems(r.Context(), run.Target, limit, offset)
	if err != nil {
		problem(w, 500, "读取媒体错误详情失败")
		return
	}
	respond(w, 200, map[string]any{"run": run, "items": items, "total": total, "offset": offset, "limit": limit})
}

func (a *App) manageRunMedia(w http.ResponseWriter, r *http.Request) {
	op := r.PathValue("operation")
	post, media := r.PathValue("post"), r.PathValue("media")
	if (op != "ignore" && op != "retry") || !validPostID(post) || media == "" || len(media) > 128 {
		problem(w, 400, "媒体操作参数无效")
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	a.scheduler.mu.Lock()
	active := a.scheduler.active
	a.scheduler.mu.Unlock()
	if active {
		problem(w, 409, "当前采集批次尚未结束，请停止采集或等待结束后再处理媒体")
		return
	}
	run, ok := a.readErrorRun(w, r)
	if !ok {
		return
	}
	err := a.store.SetMediaIgnored(run.Target, post, media, op == "ignore")
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, 404, "媒体不存在或不属于此任务账号及其引用推文")
		return
	}
	if err != nil {
		if errors.Is(err, archive.ErrMediaAlreadySaved) {
			problem(w, 409, err.Error())
		} else {
			problem(w, 500, "更新媒体状态失败")
		}
		return
	}
	a.invalidateCollectionStats(run.Target, map[string]bool{post: true})
	message := "已恢复媒体重试资格，请手动更新对应账号或等待后续自动采集；不会立即开始下载。"
	if op == "ignore" {
		message = "已忽略此媒体并停止重试，推文与错误记录保留；不会计为下载成功。历史任务记录保持不变。"
	}
	respond(w, 200, map[string]string{"message": message})
}
