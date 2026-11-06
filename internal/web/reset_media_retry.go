package web

import (
	"fmt"
	"net/http"
	"time"
)

func (a *App) resetMediaRetry(w http.ResponseWriter, r *http.Request) {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	a.scheduler.mu.Lock()
	active := a.scheduler.active
	a.scheduler.mu.Unlock()
	if active {
		problem(w, http.StatusConflict, "当前采集批次尚未结束，请停止采集或等待结束后再清除媒体重试等待")
		return
	}
	// Release only deferred transient failures. Keep attempts and diagnostics so
	// another failure still follows the configured backoff, and do not schedule
	// collection or bypass permanent failures, account pauses, or rate limits.
	result, err := a.store.DB.Exec("UPDATE media SET next_attempt=0 WHERE status='failed' AND next_attempt>?", time.Now().Unix())
	if err != nil {
		problem(w, http.StatusInternalServerError, "清除媒体重试等待失败，请稍后重试")
		return
	}
	count, err := result.RowsAffected()
	if err != nil {
		problem(w, http.StatusInternalServerError, "无法读取媒体重试等待的清除结果，请刷新后重试")
		return
	}
	message := "没有正在等待重试的媒体。"
	if count > 0 {
		message = fmt.Sprintf("已清除 %d 个媒体的重试等待。请手动更新对应账号，或等待后续自动采集；本操作不会立即开始下载。", count)
	}
	respond(w, http.StatusOK, map[string]any{"cleared": count, "message": message})
}
