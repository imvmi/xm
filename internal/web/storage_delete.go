package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"xm/internal/archive"
)

type storageReferenceReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func storageReferenceCounts(ctx context.Context, db storageReferenceReader, id string) (media, copies int64, err error) {
	err = db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM media WHERE storage_id=?),(SELECT COUNT(*) FROM media_local_copies WHERE storage_id=?)`, id, id).Scan(&media, &copies)
	return
}

func storageDeletionReason(cfg archive.Config, id string, media, copies int64, job *migrationJob, busy bool) string {
	if id == "local" {
		return "本地存储是内置连接，不能删除。"
	}
	if cfg.ActiveStorage() == id {
		return "这是当前媒体存储。请先在“迁移与清理”迁出媒体并切换保存位置。"
	}
	if media > 0 {
		return fmt.Sprintf("仍有 %d 个媒体记录使用此连接，请先迁移这些媒体。", media)
	}
	if copies > 0 {
		return fmt.Sprintf("仍有 %d 个迁移副本需要此连接校验，请先在“迁移与清理”处理本地副本。", copies)
	}
	if busy {
		return "迁移或清理正在执行，请结束后再删除连接。"
	}
	if job != nil && job.Kind == "media" && job.Status != "completed" && (job.Source == id || job.Target == id) {
		return "未完成的媒体迁移仍需要此连接，请先处理迁移任务。"
	}
	return ""
}

// Removes only an unused connection record. Never contacts the remote provider
// or deletes media. The request holds writeGate.RLock; migrations cannot cut over.
func (a *App) deleteStorageProfile(w http.ResponseWriter, r *http.Request) {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	// Keep migration eligibility stable until DB commit and config publication.
	m := a.migrations
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := a.Config()
	id := r.PathValue("id")
	if id == "local" {
		problem(w, 409, "本地存储是内置连接，不能删除。")
		return
	}
	if _, err := cfg.Storage(id); err != nil {
		problem(w, 404, "存储连接不存在")
		return
	}
	tx, err := a.store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		problem(w, 500, "无法检查存储连接，请稍后重试")
		return
	}
	defer tx.Rollback()
	media, copies, err := storageReferenceCounts(r.Context(), tx, id)
	if err != nil {
		problem(w, 500, "无法检查存储引用，连接已保留")
		return
	}
	if reason := storageDeletionReason(cfg, id, media, copies, m.job, m.running || m.closing || a.maintenance.Load()); reason != "" {
		problem(w, 409, reason)
		return
	}
	// Recheck references in the deletion statement, including all media states.
	result, err := tx.ExecContext(r.Context(), `DELETE FROM storage_profiles WHERE id=? AND NOT EXISTS (SELECT 1 FROM media WHERE storage_id=?) AND NOT EXISTS (SELECT 1 FROM media_local_copies WHERE storage_id=?) AND NOT EXISTS (SELECT 1 FROM system_settings WHERE active_storage_id=?)`, id, id, id, id)
	if err != nil {
		problem(w, 500, "删除存储连接失败，原配置保留")
		return
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		problem(w, 409, "存储连接或引用已变更，请刷新后重试")
		return
	}
	if err = tx.Commit(); err != nil {
		problem(w, 500, "删除存储连接失败，原配置保留")
		return
	}
	a.configMu.Lock()
	remaining := make([]archive.StorageProfile, 0, len(a.config.StorageProfiles))
	for _, p := range a.config.StorageProfiles {
		if p.ID != id {
			remaining = append(remaining, p)
		}
	}
	a.config.StorageProfiles = remaining
	a.configMu.Unlock()
	respond(w, 200, map[string]string{"message": "存储连接及凭据已从 XM 移除，存储中的文件未删除"})
}
