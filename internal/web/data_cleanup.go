package web

import "net/http"

type dataCleanupInput struct {
	Notifications bool `json:"notifications"`
	Tasks         bool `json:"tasks"`
	Confirm       bool `json:"confirm"`
}

type dataCleanupResult struct {
	Notifications int64 `json:"notifications"`
	Runs          int64 `json:"runs"`
	Jobs          int64 `json:"jobs"`
}

func (a *App) cleanupData(w http.ResponseWriter, r *http.Request) {
	var input dataCleanupInput
	if !decode(w, r, &input) {
		return
	}
	if !input.Notifications && !input.Tasks {
		problem(w, 400, "请选择需要清理的记录")
		return
	}
	if !input.Confirm {
		problem(w, 400, "请确认清理所选记录，此操作无法撤销")
		return
	}
	tx, err := a.store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		problem(w, 500, "无法开始清理记录，请稍后重试")
		return
	}
	defer tx.Rollback()
	var result dataCleanupResult
	remove := func(query string, count *int64) error {
		res, err := tx.ExecContext(r.Context(), query)
		if err != nil {
			return err
		}
		if count != nil {
			*count, err = res.RowsAffected()
		}
		return err
	}
	if input.Notifications {
		err = remove("DELETE FROM notification_deliveries WHERE status IN ('sent','failed','cancelled')", &result.Notifications)
	}
	if err == nil && input.Tasks {
		// A finished attempt may still belong to an active retry or queued job.
		// Keep both links intact until the entire job has finished.
		err = remove(`DELETE FROM runs WHERE status<>'running' AND NOT EXISTS (
		 SELECT 1 FROM collection_jobs j WHERE j.finished_at=0 AND (j.id=runs.job_id OR j.run_id=runs.id)
		)`, &result.Runs)
		if err == nil {
			err = remove("DELETE FROM run_metrics WHERE NOT EXISTS (SELECT 1 FROM runs WHERE runs.id=run_metrics.run_id)", nil)
		}
		if err == nil {
			err = remove(`DELETE FROM collection_jobs WHERE finished_at>0 AND NOT EXISTS (
			 SELECT 1 FROM runs WHERE runs.job_id=collection_jobs.id OR runs.id=collection_jobs.run_id
			)`, &result.Jobs)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		problem(w, 500, "清理记录失败，请稍后重试")
		return
	}
	respond(w, 200, result)
}
