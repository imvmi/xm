package web

import (
	"net/http"
	"time"

	"xm/internal/database"
)

// Serialize with job claiming so an in-flight budget cannot write stale quota
// back after the reset. Clearing local state never claims to reset X's quota.
func (a *App) resetRateLimit(w http.ResponseWriter, r *http.Request) {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	a.scheduler.mu.Lock()
	active := a.scheduler.active
	a.scheduler.mu.Unlock()
	if active {
		problem(w, http.StatusConflict, "当前采集批次尚未结束，请停止采集或等待结束后再清空限流状态")
		return
	}
	tx, err := a.store.DB.Begin()
	if err != nil {
		problem(w, 500, "清空限流状态失败")
		return
	}
	defer tx.Rollback()
	if err = saveRateLimit(tx, 0, 0); err == nil {
		_, err = tx.Exec("DELETE FROM request_budgets")
	}
	for _, key := range []string{"request_budget_until", "budget_batch_failed"} {
		if err != nil {
			break
		}
		_, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), key, "0")
	}
	// Keep checkpoints, unfinished batch membership, manual queue and pause
	// preferences. Remove the delayed schedule as well as its cooldown banner.
	if err == nil {
		_, err = tx.Exec("UPDATE monitors SET next_run=? WHERE enabled=1", time.Now().Unix())
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		problem(w, 500, "清空限流状态失败")
		return
	}
	a.scheduler.Wake()
	respond(w, 200, map[string]string{"message": "本地限流状态已清空，任务将按启用和暂停设置继续执行；X 服务器仍可能返回限流。"})
}
