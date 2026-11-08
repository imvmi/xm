package archive

import (
	"context"
	"errors"
	"sync"
)

// MediaPipeline overlaps page collection with one serial downloader. The channel
// is only a coalesced wake-up: the database remains the durable work queue.
// The owner must call Finish after the last page, including on collection error.
type MediaPipeline struct {
	wake    chan struct{}
	finish  chan struct{}
	done    chan struct{}
	once    sync.Once
	summary DownloadSummary
	err     error
}

func StartMediaPipeline(ctx context.Context, download func(context.Context) (DownloadSummary, error)) *MediaPipeline {
	p := &MediaPipeline{wake: make(chan struct{}, 1), finish: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		changed := map[string]bool{}
		run := func() {
			if ctx.Err() != nil {
				p.err = ctx.Err()
				return
			}
			summary, err := download(ctx)
			p.summary.Downloaded += summary.Downloaded
			p.summary.Skipped += summary.Skipped
			p.summary.Failed += summary.Failed
			for _, id := range summary.ChangedPostIDs {
				if !changed[id] {
					changed[id] = true
					p.summary.ChangedPostIDs = append(p.summary.ChangedPostIDs, id)
				}
			}
			var failures *MediaDownloadError
			if err != nil && !errors.As(err, &failures) {
				p.err = err
			}
		}
		for {
			select {
			case <-ctx.Done():
				p.err = ctx.Err()
				return
			case <-p.finish:
				// One final database snapshot after collection commits its last page also
				// covers wake-ups coalesced while a large file was downloading.
				run()
				if p.err == nil && p.summary.Failed > 0 {
					p.err = &MediaDownloadError{Count: p.summary.Failed}
				}
				return
			case <-p.wake:
				run()
			}
		}
	}()
	p.Notify()
	return p
}

// Notify never waits for the download or for space in a page queue.
func (p *MediaPipeline) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Finish drains currently eligible media, without waiting for future retries.
// Cancellation/deadline applies to both collection and download. Joining here
// prevents downloads from escaping the task's storage/configuration lifetime.
func (p *MediaPipeline) Finish() (DownloadSummary, error) {
	p.once.Do(func() { close(p.finish) })
	<-p.done
	return p.summary, p.err
}
