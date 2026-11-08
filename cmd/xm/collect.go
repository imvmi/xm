package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"xm/internal/archive"
)

type collectServices struct {
	collect  func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error)
	download func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error)
	avatar   func(context.Context, archive.Config, archive.Profile) error
}

type cliCollectResult struct {
	Collection archive.Collection      `json:"collection"`
	Media      archive.DownloadSummary `json:"media"`
	Status     string                  `json:"status"`
}

func collectTarget(parent context.Context, s *archive.Store, cfg archive.Config, cookies archive.Cookies, target string, limit int, replies, backfill bool, services collectServices) (out cliCollectResult, runErr error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.BrowserMaxAge)*time.Second)
	defer cancel()
	id, err := s.StartRun(target)
	if err != nil {
		return out, err
	}
	out.Status = "failed"
	message := ""
	defer func() {
		if message == "" && runErr != nil {
			message = runErr.Error()
		}
		if err := s.EndRun(id, out.Status, out.Collection.Posts, message); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	identity, err := s.TargetIdentity(target)
	if err != nil {
		return out, err
	}
	if !backfill {
		identity.KnownPostIDs, err = s.KnownTargetPosts(target, max(limit*4, 500))
		if err != nil {
			return out, err
		}
	}
	media := archive.StartMediaPipeline(ctx, func(ctx context.Context) (archive.DownloadSummary, error) {
		return services.download(ctx, s, cfg, target, false)
	})
	var latestProfile *archive.Profile
	saveFailed := false
	save := func(page archive.Page) error {
		if page.Profile != nil {
			if err := s.SaveTargetProfile(target, *page.Profile); err != nil {
				saveFailed = true
				return err
			}
			profile := *page.Profile
			latestProfile = &profile
		}
		if err := s.SavePosts(target, page.Posts); err != nil {
			saveFailed = true
			return err
		}
		media.Notify()
		if len(page.Posts) > 0 {
			fmt.Fprintf(os.Stderr, "@%s 已入库本页 %d 条\n", target, len(page.Posts))
		}
		return nil
	}
	var collectErr error
	out.Collection, collectErr = services.collect(ctx, cfg, cookies, target, identity, limit, replies, save)
	if latestProfile != nil && latestProfile.AvatarURL != "" && ctx.Err() == nil {
		_ = services.avatar(ctx, cfg, *latestProfile)
	}
	var mediaErr error
	out.Media, mediaErr = media.Finish()
	// Always inspect the durable queue, including files skipped for retry timing
	// or permanent failure. A batch with no eligible files is not proof of success.
	remaining, remainingErr := s.OutstandingMedia(target)
	if parent.Err() != nil {
		out.Status = "cancelled"
		message = "任务已停止，已保存内容和未完成媒体保留"
		return out, parent.Err()
	}
	if !saveFailed && errors.Is(ctx.Err(), context.DeadlineExceeded) && (collectErr == nil || archive.FailureKind(collectErr) == "transient" || errors.Is(collectErr, context.DeadlineExceeded)) {
		out.Status = "incomplete"
		message = "本轮任务时间已用完，已保存内容和未完成媒体保留，请继续采集或重试媒体"
		return out, context.DeadlineExceeded
	}
	if collectErr != nil {
		return out, collectErr
	}
	if remainingErr != nil {
		return out, remainingErr
	}
	var failures *archive.MediaDownloadError
	if remaining == 0 && errors.As(mediaErr, &failures) {
		mediaErr = nil
	}
	if remaining > 0 || mediaErr != nil {
		out.Status = "media_failed"
		message = fmt.Sprintf("仍有 %d 个媒体未完成归档（含等待重试或永久失败）", remaining)
		if mediaErr != nil {
			message += "；" + mediaErr.Error()
		}
		return out, errors.New(message)
	}
	out.Status = "bounded_complete"
	message = out.Collection.Stop
	return out, nil
}
