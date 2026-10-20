package web

import (
	"context"
	"fmt"
)

func (m *migrationManager) cleanupLocalCopies(ctx context.Context) error {
	items, err := m.app.store.LocalMediaCopies(ctx)
	if err != nil {
		return err
	}
	var size int64
	for _, item := range items {
		size += item.Bytes
	}
	if err = m.update(func(j *migrationJob) {
		j.Total, j.TotalBytes = int64(len(items)), size
		j.Done, j.Bytes, j.Failed = 0, 0, 0
		j.Phase = "重新校验远端并清理本地副本"
	}); err != nil {
		return err
	}
	var failed int
	for _, item := range items {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = m.app.store.CleanupLocalMediaCopy(ctx, m.app.Config(), item)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			failed++
		}
		if updateErr := m.update(func(j *migrationJob) {
			if err == nil {
				j.Done++
				j.Bytes += item.Bytes
			} else {
				j.Failed++
				if len(j.Failures) < 100 {
					j.Failures = append(j.Failures, migrationFailure{item.PostID, item.MediaID, err.Error()})
				}
			}
		}); updateErr != nil {
			return updateErr
		}
	}
	if failed != 0 {
		return fmt.Errorf("%d 个本地副本未通过清理检查或删除失败，文件保留；请检查后重试。", failed)
	}
	return nil
}
