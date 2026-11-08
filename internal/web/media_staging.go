package web

import (
	"errors"
	"net/http"

	"xm/internal/archive"
)

func (a *App) mediaStagingUsage(w http.ResponseWriter, r *http.Request) {
	cfg := a.Config()
	usage, err := a.store.MediaStagingUsage(r.Context(), cfg.DataDir)
	if err != nil {
		problem(w, 500, "读取下载暂存占用失败，请检查数据目录权限")
		return
	}
	respond(w, 200, map[string]any{"usage": usage, "limit_bytes": archive.MediaStagingLimit(cfg)})
}

func (a *App) clearMediaPartials(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		problem(w, 400, "请确认清理断点，尚未下载完整的文件之后需要从头下载")
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	a.scheduler.mu.Lock()
	active := a.scheduler.active
	a.scheduler.mu.Unlock()
	if active {
		problem(w, 409, "当前采集批次尚未结束，请停止采集或等待结束后再清理断点")
		return
	}
	removed, err := a.store.ClearMediaPartials(r.Context(), a.Config().DataDir)
	if errors.Is(err, archive.ErrMediaStagingBusy) {
		problem(w, 409, err.Error())
		return
	}
	if err != nil {
		problem(w, 500, "断点清理未全部完成，请刷新占用后重试")
		return
	}
	respond(w, 200, map[string]any{"removed_bytes": removed, "message": "下载断点已清理。已归档媒体和已下载完整的待上传文件保留；重试时间保持不变。"})
}
