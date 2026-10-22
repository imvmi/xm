package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xm/internal/archive"
)

type collector func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error)
type downloader func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error)
type avatarDownloader func(context.Context, archive.Config, archive.Profile) error
type collectionSession interface {
	CollectBound(context.Context, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error)
	Healthy(context.Context) error
	Close()
}
type sessionFactory func(context.Context, archive.Config, archive.Cookies) (collectionSession, error)

type Scheduler struct {
	app        *App
	wake       chan struct{}
	mu         sync.Mutex
	current    string
	cancel     context.CancelFunc
	active     bool
	collect    collector
	newSession sessionFactory
	download   downloader
	avatar     avatarDownloader
	lastError  string
	waitUntil  int64
	lastFinish time.Time
	browser    schedulerBrowser
	progress   taskProgress
}

type taskProgress struct {
	jobID             int64
	phase             string
	started, deadline time.Time
}

func (s *Scheduler) setPhase(phase string) {
	s.mu.Lock()
	s.progress.phase = phase
	s.mu.Unlock()
}

func (s *Scheduler) describeProgress(job *CollectionJob) {
	s.mu.Lock()
	p := s.progress
	s.mu.Unlock()
	if p.jobID != job.ID {
		return
	}
	job.Phase = p.phase
	job.StartedAt = p.started.Unix()
	job.Elapsed = max(0, int64(time.Since(p.started)/time.Second))
	remaining := max(0, int64(time.Until(p.deadline)/time.Second))
	job.Remaining = &remaining
}

func newScheduler(a *App) *Scheduler {
	return &Scheduler{app: a, wake: make(chan struct{}, 1), newSession: func(ctx context.Context, cfg archive.Config, cookies archive.Cookies) (collectionSession, error) {
		return archive.NewBrowserSession(ctx, cfg, cookies)
	}, download: archive.DownloadPending, avatar: archive.DownloadAvatar}
}
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Scheduler) Current() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.lastError
}
func (s *Scheduler) WaitingUntil() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitUntil
}
func (s *Scheduler) lastTaskFinish() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastFinish.IsZero() {
		return s.lastFinish
	}
	var finished int64
	if err := s.app.store.DB.QueryRow("SELECT COALESCE(MAX(finished_at),0) FROM collection_jobs WHERE status<>'cancelled'").Scan(&finished); err == nil && finished > 0 {
		s.lastFinish = time.Unix(finished, 0)
	}
	return s.lastFinish
}
func (s *Scheduler) recordTaskFinish(finished time.Time) {
	s.mu.Lock()
	s.lastFinish = finished
	s.mu.Unlock()
}
func (s *Scheduler) Cancel(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == target && s.cancel != nil {
		s.cancel()
	}
}
func (s *Scheduler) Run(ctx context.Context) {
	defer s.closeSession()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		worked, err := s.step(ctx)
		if err != nil {
			s.mu.Lock()
			s.lastError = "调度失败：" + safeMessage(err)
			s.mu.Unlock()
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// waitBetweenJobs anchors the delay to the preceding task's completion. A
// settings save wakes this loop so shortening, extending, or disabling the
// interval takes effect before the next target starts.
func (s *Scheduler) waitBetweenJobs(ctx context.Context, completedAt time.Time) bool {
	defer func() {
		s.mu.Lock()
		s.waitUntil = 0
		s.mu.Unlock()
	}()
	for {
		if s.app.maintenance.Load() {
			return false
		}
		interval := s.app.Config().QueueInterval
		deadline := completedAt.Add(time.Duration(interval) * time.Second)
		if interval == 0 || !time.Now().Before(deadline) {
			return true
		}
		s.mu.Lock()
		s.waitUntil = deadline.Unix()
		s.mu.Unlock()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false
		case <-s.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			return true
		}
	}
}

func (s *Scheduler) step(ctx context.Context) (bool, error) {
	a := s.app
	if !a.writeGate.TryRLock() {
		return false, nil
	}
	defer a.writeGate.RUnlock()
	if a.maintenance.Load() {
		return false, nil
	}
	a.changeMu.Lock()
	cfg := a.Config()
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active || time.Now().Before(s.browser.retryAt) {
		a.changeMu.Unlock()
		return false, nil
	}
	if state(a.store.DB, "cookie_blocked") != "" {
		a.changeMu.Unlock()
		return false, nil
	}
	until, _ := strconv.ParseInt(state(a.store.DB, "cooldown_until"), 10, 64)
	if time.Now().Unix() < until {
		a.changeMu.Unlock()
		return false, nil
	}
	cookies, err := a.store.LoadCookies()
	if err != nil {
		a.changeMu.Unlock()
		return false, nil
	}
	// Settings/import operations and claiming a job are serialized. A replacement
	// cookie or proxy always takes effect for the next job, never halfway through one.
	cookieGeneration := state(a.store.DB, "cookie_imported")
	now := time.Now().Unix()
	var pending []string
	rawPending := state(a.store.DB, "budget_batch_pending")
	if rawPending != "" {
		if err = json.Unmarshal([]byte(rawPending), &pending); err != nil {
			a.changeMu.Unlock()
			return false, err
		}
	}
	autoBatch := false
	if !cfg.PollingPaused {
		var due int
		if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM monitors WHERE enabled=1 AND running=0 AND next_run<=?", now).Scan(&due); err != nil {
			a.changeMu.Unlock()
			return false, err
		}
		// An unfinished batch resumes as soon as its cooldown permits, even
		// when its last target already has a future next_run from an earlier slice.
		autoBatch = rawPending != "" || due > 0
	}
	condition := "m.requested=1 OR EXISTS (SELECT 1 FROM collection_jobs q WHERE q.target=m.username AND q.finished_at=0)"
	if cfg.PollingPaused {
		condition = "m.requested=1 OR EXISTS (SELECT 1 FROM collection_jobs q WHERE q.target=m.username AND q.finished_at=0 AND q.source IN ('manual','api'))"
	}
	var jobArgs []any
	if autoBatch {
		if rawPending == "" {
			condition = "m.enabled=1 OR m.requested=1 OR EXISTS (SELECT 1 FROM collection_jobs q WHERE q.target=m.username AND q.finished_at=0)"
		} else if len(pending) > 0 {
			condition = "m.requested=1 OR EXISTS (SELECT 1 FROM collection_jobs q WHERE q.target=m.username AND q.finished_at=0) OR (m.enabled=1 AND m.username IN (" + strings.TrimRight(strings.Repeat("?,", len(pending)), ",") + "))"
			for _, target := range pending {
				jobArgs = append(jobArgs, target)
			}
		}
	}
	rows, err := a.store.DB.Query("SELECT "+monitorColumns+" FROM monitors m WHERE m.running=0 AND ("+condition+") ORDER BY m.last_started,m.requested DESC,m.username", jobArgs...)
	if err != nil {
		a.changeMu.Unlock()
		return false, err
	}
	jobs := []Monitor{}
	for rows.Next() {
		m, scanErr := scanMonitor(rows)
		if scanErr != nil {
			rows.Close()
			a.changeMu.Unlock()
			return false, scanErr
		}
		jobs = append(jobs, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		a.changeMu.Unlock()
		return false, err
	}
	queueJobs := make(map[string]CollectionJob, len(jobs))
	for _, job := range jobs {
		source := "automatic"
		if job.Requested {
			source = "manual"
		}
		queuedJob, queueErr := ensureCollectionJob(a.store.DB, job.Username, source, automaticCollectionMode(job))
		if queueErr != nil {
			a.changeMu.Unlock()
			return false, queueErr
		}
		queueJobs[job.Username] = queuedJob
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		left, right := queueJobs[jobs[i].Username], queueJobs[jobs[j].Username]
		leftReady, rightReady := left.RetryAt <= now, right.RetryAt <= now
		if leftReady != rightReady {
			return leftReady
		}
		if !leftReady && left.RetryAt != right.RetryAt {
			return left.RetryAt < right.RetryAt
		}
		if left.QueueOrder == right.QueueOrder {
			return left.ID < right.ID
		}
		return left.QueueOrder < right.QueueOrder
	})
	if len(jobs) == 0 {
		if autoBatch {
			err = s.finishBatch()
		}
		a.changeMu.Unlock()
		return false, err
	}
	if autoBatch {
		// One durable entry per target, shared by automatic and manual requests.
		// Repeated ticks cannot append another copy of the same batch.
		pending = make([]string, 0, len(jobs))
		for _, m := range jobs {
			pending = append(pending, m.Username)
		}
		if err = s.savePending(pending); err != nil {
			a.changeMu.Unlock()
			return false, err
		}
	}
	if len(jobs) > 0 && queueJobs[jobs[0].Username].RetryAt > now {
		a.changeMu.Unlock()
		return false, nil
	}
	s.mu.Lock()
	s.active = true
	s.lastError = ""
	s.mu.Unlock()
	a.changeMu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = false
		s.current = ""
		s.cancel = nil
		s.mu.Unlock()
	}()

	batchCtx, cancelBatch := context.WithCancel(ctx)
	defer cancelBatch()
	collect := s.collect
	if collect == nil {
		collect = func(parent context.Context, cfg archive.Config, cookies archive.Cookies, target string, identity archive.TargetIdentity, limit int, replies bool, save func(archive.Page) error) (archive.Collection, error) {
			return s.collectWithSession(ctx, parent, cfg, cookies, target, identity, limit, replies, save)
		}
	}
	processed := []string{}
	globalStop := false
	rateStopped := false
	batchSuccessful := true
	batchHadFailure := autoBatch && state(a.store.DB, "budget_batch_failed") == "1"
	interrupted := false
	lastFinished := s.lastTaskFinish()
	needInterval := !lastFinished.IsZero()
	for _, queued := range jobs {
		if ctx.Err() != nil || a.maintenance.Load() {
			interrupted = true
			break
		}
	boundary:
		// Re-read the target and settings at each boundary; the queued snapshot may
		// have been edited, paused, or deleted while the preceding target ran.
		a.changeMu.Lock()
		cfg = a.Config()
		queueJob, jobErr := scanCollectionJob(a.store.DB.QueryRow("SELECT "+collectionJobColumns+" FROM collection_jobs WHERE id=?", queueJobs[queued.Username].ID))
		if errors.Is(jobErr, sql.ErrNoRows) || jobErr == nil && queueJob.FinishedAt != 0 {
			a.changeMu.Unlock()
			if err = s.updatePending(queued.Username, false); err != nil {
				return true, err
			}
			continue
		}
		if jobErr != nil {
			a.changeMu.Unlock()
			return true, jobErr
		}
		if queueJob.RetryAt > time.Now().Unix() {
			a.changeMu.Unlock()
			interrupted = true
			continue
		}
		m, loadErr := scanMonitor(a.store.DB.QueryRow("SELECT "+monitorColumns+" FROM monitors m WHERE m.username=?", queued.Username))
		if errors.Is(loadErr, sql.ErrNoRows) || loadErr == nil && (!m.Enabled && !m.Requested && queueJob.Source != "manual" && queueJob.Source != "api") {
			if jobErr = cancelMonitorJobs(a.store.DB, queued.Username, "监控已暂停或移除"); jobErr != nil {
				a.changeMu.Unlock()
				return true, jobErr
			}
			a.changeMu.Unlock()
			if err = s.updatePending(queued.Username, false); err != nil {
				return true, err
			}
			continue
		}
		if loadErr != nil {
			a.changeMu.Unlock()
			return true, loadErr
		}
		if queueJob.Mode == collectionModeIncremental && automaticCollectionMode(m) == collectionModeBackfill {
			queueJob.Mode = collectionModeBackfill
		}
		if cfg.PollingPaused && !m.Requested && queueJob.Source != "manual" && queueJob.Source != "api" {
			a.changeMu.Unlock()
			interrupted = true
			continue
		}
		if needInterval {
			a.changeMu.Unlock()
			if !s.waitBetweenJobs(batchCtx, lastFinished) {
				interrupted = true
				break
			}
			needInterval = false
			goto boundary
		}
		latestCookies, loadErr := a.store.LoadCookies()
		if loadErr != nil {
			a.changeMu.Unlock()
			return true, loadErr
		}
		if latestCookies != cookies {
			cookies = latestCookies
		}
		cookieGeneration = state(a.store.DB, "cookie_imported")
		if state(a.store.DB, "cookie_blocked") != "" {
			a.changeMu.Unlock()
			interrupted = true
			break
		}
		startedAt := time.Now()
		started := startedAt.Unix()
		claim, claimErr := a.store.DB.Begin()
		if claimErr != nil {
			a.changeMu.Unlock()
			return true, claimErr
		}
		if _, claimErr = claim.Exec("UPDATE collection_jobs SET mode=? WHERE id=? AND finished_at=0", queueJob.Mode, queueJob.ID); claimErr == nil {
			claimErr = markCollectionJobRunning(claim, queueJob.ID, started)
		}
		if claimErr == nil {
			_, claimErr = claim.Exec("UPDATE monitors SET running=1,requested=0,last_started=?,last_error='' WHERE username=?", started, m.Username)
		}
		if claimErr == nil && queueJob.Mode != collectionModeIncremental {
			_, claimErr = claim.Exec(`UPDATE monitors SET baseline_status='backfilling',posts_baseline_status='backfilling',replies_baseline_status=CASE WHEN replies=1 THEN 'backfilling' ELSE 'not_enabled' END,baseline_started=CASE WHEN baseline_started=0 OR ?='rescan' THEN ? ELSE baseline_started END,baseline_finished=0,baseline_stop_reason='' WHERE username=?`, queueJob.Mode, started, m.Username)
		}
		if claimErr == nil {
			claimErr = claim.Commit()
		} else {
			_ = claim.Rollback()
		}
		if claimErr != nil {
			a.changeMu.Unlock()
			return true, claimErr
		}
		targetCtx, cancelTarget := context.WithCancel(batchCtx)
		s.mu.Lock()
		s.current, s.cancel = m.Username, cancelTarget
		s.mu.Unlock()
		a.changeMu.Unlock()
		log.Printf("collection start target=%s mode=%s catchup=%t", m.Username, queueJob.Mode, m.Catchup)
		kind, runID, runErr := s.runMonitor(targetCtx, cfg, cookies, cookieGeneration, m, queueJob.ID, queueJob.Mode, collect)
		targetCancelled := targetCtx.Err() != nil
		cancelTarget()
		s.mu.Lock()
		s.current = ""
		s.cancel = nil
		s.mu.Unlock()
		processed = append(processed, m.Username)
		if kind != "" && kind != "refresh_head" || targetCancelled {
			batchSuccessful = false
			batchHadFailure = true
		}
		log.Printf("collection finish target=%s kind=%s duration=%s", m.Username, kind, time.Since(startedAt).Round(time.Millisecond))
		lastFinished = time.Now()
		s.recordTaskFinish(lastFinished)
		needInterval = true
		if autoBatch && batchHadFailure {
			if err = setState(a.store.DB, "budget_batch_failed", "1"); err != nil {
				return true, err
			}
		}
		if runErr != nil {
			_ = finishCollectionJob(a.store.DB, queueJob.ID, runID, "retrying", safeMessage(runErr), true)
			return true, runErr
		}
		if kind == "budget_storage" {
			return true, errors.New("请求间隔状态无法保存，已停止发送采集请求")
		}
		if kind == "auth" && !targetCancelled {
			if err = a.recoverCookie(batchCtx, cookieGeneration, m.Username); err != nil {
				return true, err
			}
		}
		retryTarget := !targetCancelled && (kind == "auth" || kind == "auth_replaced" || kind == "rate" || kind == "browser" || kind == "timeline" || kind == "refresh_head")
		if ctx.Err() != nil {
			retryTarget = true // Service shutdown retains work; a user cancellation does not.
			interrupted = true
			if m.Requested {
				if _, err = a.store.DB.Exec("UPDATE monitors SET requested=1 WHERE username=?", m.Username); err != nil {
					return true, err
				}
			}
		}
		jobStatus, jobMessage := "completed", "任务已完成"
		if runID > 0 {
			var runStatus string
			if queryErr := a.store.DB.QueryRow("SELECT status,message FROM runs WHERE id=?", runID).Scan(&runStatus, &jobMessage); queryErr == nil {
				if runStatus == "failed" || runStatus == "media_failed" {
					jobStatus = "failed"
				} else if runStatus == "incomplete" {
					jobStatus = "incomplete"
				}
			}
		}
		if targetCancelled {
			jobStatus, jobMessage = "cancelled", "任务已由用户停止"
		} else if retryTarget {
			jobStatus = retryJobStatus(kind)
			if kind == "timeline" {
				retryCount, delay := timelineRetryBackoff(queueJob.RetryCount)
				jobMessage = fmt.Sprintf("%s；%d 秒后从已保存断点重试", jobMessage, int(delay/time.Second))
				if err = retryTimelineCollectionJob(a.store.DB, queueJob.ID, runID, jobMessage, time.Now().Add(delay).Unix(), retryCount); err != nil {
					return true, err
				}
			}
			if ctx.Err() != nil {
				jobStatus, jobMessage = "retrying", "服务重启后继续执行"
			}
		}
		if kind != "timeline" || !retryTarget {
			if err = finishCollectionJob(a.store.DB, queueJob.ID, runID, jobStatus, jobMessage, retryTarget); err != nil {
				return true, err
			}
		}
		if err = s.updatePending(m.Username, retryTarget); err != nil {
			return true, err
		}
		if retryTarget {
			globalStop = true
			rateStopped = kind == "rate"
			break
		}
	}
	if autoBatch {
		if state(a.store.DB, "budget_batch_pending") == "[]" && !interrupted {
			a.changeMu.Lock()
			err = s.finishBatch()
			a.changeMu.Unlock()
			if err != nil {
				return true, err
			}
		} else if rateStopped {
			until, _ := strconv.ParseInt(state(a.store.DB, "cooldown_until"), 10, 64)
			if _, err = a.store.DB.Exec("UPDATE monitors SET next_run=CASE WHEN next_run<? THEN ? ELSE next_run END WHERE enabled=1", until, until); err != nil {
				return true, err
			}
		}
	}
	// A single manual success is not evidence that the whole batch can run.
	if autoBatch && !globalStop && !interrupted && batchSuccessful && !batchHadFailure && len(processed) > 0 {
		tx, err := a.store.DB.Begin()
		if err != nil {
			return true, err
		}
		defer tx.Rollback()
		if err = saveRateLimit(tx, 0, 0); err != nil {
			return true, err
		}
		if err = tx.Commit(); err != nil {
			return true, err
		}
	}
	return len(processed) > 0, nil
}

func (s *Scheduler) runMonitor(jobCtx context.Context, cfg archive.Config, cookies archive.Cookies, cookieGeneration string, m Monitor, jobID int64, mode string, collect collector) (string, int64, error) {
	defer s.app.invalidateStats()
	a := s.app
	// Browser lifetime is also the task budget for collection, avatars and media together.
	// Keep the parent separate so exhausting this budget is not a user stop.
	workCtx, cancelWork := context.WithTimeout(jobCtx, time.Duration(cfg.BrowserMaxAge)*time.Second)
	defer cancelWork()
	deadline, _ := workCtx.Deadline()
	s.mu.Lock()
	s.progress = taskProgress{jobID: jobID, phase: "preparing", started: time.Now(), deadline: deadline}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.progress = taskProgress{}
		s.mu.Unlock()
	}()
	if err := a.prepareNotifications(m); err != nil {
		_, _ = a.store.DB.Exec("UPDATE monitors SET running=0 WHERE username=?", m.Username)
		return "", 0, err
	}
	id, err := a.store.StartJobRun(m.Username, jobID)
	if err != nil {
		_, _ = a.store.DB.Exec("UPDATE monitors SET running=0 WHERE username=?", m.Username)
		return "", 0, err
	}
	if _, err = a.store.DB.Exec("UPDATE collection_jobs SET run_id=? WHERE id=? AND finished_at=0", id, jobID); err != nil {
		return "", id, err
	}
	if _, err = a.store.DB.Exec("INSERT INTO run_metrics(run_id,new_posts,posts_read) VALUES(?,0,0)", id); err != nil {
		return "", id, err
	}
	identity, collectErr := a.store.TargetIdentity(m.Username)
	if collectErr == nil && m.Catchup {
		identity.Progress, collectErr = a.store.LoadCollectionProgress(m.Username, identity.UserID)
	}
	identity.LookupKnownPosts = func(ids []string) (map[string]bool, error) {
		return a.store.KnownTargetPostIDs(workCtx, m.Username, ids)
	}
	postAnchor, replyAnchor := m.CatchupPost, m.CatchupReply
	if collectErr == nil && !m.Catchup {
		postAnchor, replyAnchor, collectErr = a.store.TargetAnchors(m.Username)
		if collectErr == nil {
			// Commit the pre-run boundary before the collector can save any posts.
			// Keep this invocation's normal incremental mode; after a crash the
			// next invocation resumes against these stable anchors instead.
			_, collectErr = a.store.DB.Exec("UPDATE monitors SET catchup=1,catchup_post_anchor=?,catchup_reply_anchor=? WHERE username=?", postAnchor, replyAnchor, m.Username)
		}
	}
	identity.Catchup = m.Catchup
	identity.DurableProgress = true
	identity.CatchupPostAnchor = postAnchor
	identity.CatchupReplyAnchor = replyAnchor
	identity.CatchupScrollBudget = m.CatchupScrolls
	identity.CollectionMode = mode
	var result archive.Collection
	var latestProfile *archive.Profile
	saveFailed := false
	if collectErr == nil {
		s.setPhase("timeline")
		result, collectErr = collect(workCtx, cfg, cookies, m.Username, identity, archive.CollectionSafetyMaxPosts, m.Replies, func(p archive.Page) error {
			if p.Profile != nil {
				if err := a.store.SaveTargetProfile(m.Username, *p.Profile); err != nil {
					saveFailed = true
					return err
				}
				profile := *p.Profile
				latestProfile = &profile
			}
			err := a.store.SaveCollectionPage(m.Username, id, p)
			saveFailed = saveFailed || err != nil
			return err
		})
	}
	if !saveFailed && collectErr != nil && archive.FailureKind(collectErr) == "transient" && jobCtx.Err() == nil && errors.Is(workCtx.Err(), context.DeadlineExceeded) {
		result.Stop, collectErr = "time_budget_reached", nil
	}
	if workCtx.Err() == nil && latestProfile != nil && latestProfile.AvatarURL != "" {
		s.setPhase("avatar")
		// An unavailable avatar is cosmetic and must never fail an otherwise
		// successful archive run; the UI keeps its generated fallback.
		_ = s.avatar(workCtx, cfg, *latestProfile)
	}

	var mediaErr error
	if workCtx.Err() == nil {
		s.setPhase("media")
		_, mediaErr = s.download(workCtx, a.store, cfg, m.Username, false)
	}
	s.setPhase("finishing")
	remainingMedia, remainingErr := a.store.OutstandingMedia(m.Username)
	mediaBudgetReached := jobCtx.Err() == nil && errors.Is(workCtx.Err(), context.DeadlineExceeded) && remainingMedia > 0
	if mediaBudgetReached {
		// Pending media stays durable and will be retried by a later task. Budget
		// exhaustion is incomplete work, not an authentication or media incident.
		mediaErr = nil
	} else if mediaErr == nil && remainingMedia > 0 {
		mediaErr = fmt.Errorf("仍有 %d 个媒体未完成归档（含等待重试或永久失败），请检查媒体状态", remainingMedia)
	}
	if remainingErr != nil {
		mediaErr = remainingErr
	}
	ended := time.Now().Unix()
	status, message := "bounded_complete", result.Stop
	bounded := result.Stop == "limit_reached" || result.Stop == "max_scrolls_reached" || result.Stop == "page_budget_reached" || result.Stop == "time_budget_reached"
	refreshHead := collectErr == nil && !bounded && result.NeedsHeadRefresh
	complete := collectErr == nil && !bounded && !refreshHead
	if collectErr == nil && bounded {
		status = "incomplete"
		message = incompleteCollectionMessage
		if result.Stop == "page_budget_reached" {
			message = "本轮已保存分页断点，下轮从未完成的位置继续"
		}
		if result.Stop == "time_budget_reached" {
			message = "本轮采集时间已用完，已保存的断点将在下轮继续"
		}
	}
	if refreshHead {
		status, message = "incomplete", "历史断点已处理完，已安排检查补采期间的新推文"
		postAnchor, replyAnchor = result.RefreshPostAnchor, result.RefreshReplyAnchor
	}
	baselineStatus, postsBaseline, repliesBaseline := m.BaselineStatus, m.PostsBaseline, m.RepliesBaseline
	baselineFinished := m.BaselineFinished
	baselineStop := m.BaselineStop
	if mode != collectionModeIncremental {
		baselineStatus, postsBaseline = "backfilling", "backfilling"
		if m.Replies {
			repliesBaseline = "backfilling"
		} else {
			repliesBaseline = "not_enabled"
		}
		baselineFinished = 0
		baselineStop = result.Stop
		failureKind := archive.FailureKind(collectErr)
		switch {
		case collectErr == nil && jobCtx.Err() == nil && result.HistoryComplete:
			baselineStatus, postsBaseline = "visible_history_complete", "visible_history_complete"
			if m.Replies {
				repliesBaseline = "visible_history_complete"
			}
			baselineFinished = ended
		case failureKind == "access" || failureKind == "identity" || failureKind == "checkpoint":
			baselineStatus, postsBaseline = "blocked", "blocked"
			if m.Replies {
				repliesBaseline = "blocked"
			}
			baselineFinished = ended
			baselineStop = message
		case collectErr == nil && jobCtx.Err() == nil && !bounded:
			// A stable browser bottom without an API end marker is useful evidence,
			// but it cannot prove that X exposed the complete account history.
			baselineStatus, postsBaseline = "partial", "partial"
			if m.Replies {
				repliesBaseline = "partial"
			}
			baselineFinished = ended
		}
	}
	failure := collectErr
	if failure == nil {
		failure = mediaErr
	}
	if failure != nil {
		status = "failed"
		message = safeMessage(failure)
	}
	if collectErr == nil && mediaErr != nil {
		status = "media_failed"
	}
	if collectErr == nil && mediaErr == nil && mediaBudgetReached {
		status = "incomplete"
		message = fmt.Sprintf("本轮任务时间已用完，已保存正文与断点；还有 %d 个媒体将在后续任务继续处理", remainingMedia)
	}
	if jobCtx.Err() != nil {
		status = "cancelled"
		message = "任务已停止；已入库内容保留"
		failure = jobCtx.Err()
	}
	failures := 0
	if failure != nil {
		failures = m.Failures + 1
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	var currentReplies bool
	var currentBaseline string
	if err = a.store.DB.QueryRow("SELECT replies,baseline_status FROM monitors WHERE username=?", m.Username).Scan(&currentReplies, &currentBaseline); err != nil {
		return "", id, err
	}
	// An edit can invalidate the scope while the old task is still collecting.
	// Its completion and late page saves must not overwrite the new baseline.
	scopeChanged := currentReplies != m.Replies || currentBaseline == "not_started"
	if scopeChanged {
		baselineStatus, postsBaseline, repliesBaseline = "not_started", "not_started", "not_enabled"
		if currentReplies {
			repliesBaseline = "not_started"
		}
		baselineFinished, baselineStop = 0, ""
		complete, refreshHead = false, false
		if failure == nil && jobCtx.Err() == nil {
			status, message = "incomplete", "采集范围已变更，下轮重新建立历史基线；已归档内容保留"
		}
	}
	if collectErr == nil && jobCtx.Err() == nil && state(a.store.DB, "cookie_imported") == cookieGeneration {
		if err = setState(a.store.DB, "cookie_api_attempted", ""); err != nil {
			return "", id, err
		}
	}
	if archive.FailureKind(collectErr) == "auth" && state(a.store.DB, "cookie_imported") == cookieGeneration {
		if err = setState(a.store.DB, "cookie_blocked", message); err != nil {
			return "", id, err
		}
	}
	if archive.FailureKind(collectErr) == "access" || archive.FailureKind(collectErr) == "identity" || archive.FailureKind(collectErr) == "checkpoint" {
		if _, err = a.store.DB.Exec("UPDATE monitors SET enabled=0,requested=0 WHERE username=?", m.Username); err != nil {
			return "", id, err
		}
	}
	rateUntil, rateFailures := int64(0), 0
	if archive.FailureKind(collectErr) == "rate" {
		previous, _ := strconv.Atoi(state(a.store.DB, "rate_limit_failures"))
		latest := a.Config()
		rateUntil, rateFailures = rateLimitDeadline(ended, latest.DefaultInterval, previous, collectErr, latest.RateLimitMode)
	}
	var count int
	_ = a.store.DB.QueryRow("SELECT COUNT(*) FROM target_posts WHERE target=?", m.Username).Scan(&count)
	lastError := ""
	if failure != nil {
		lastError = message
	} else if !complete || mediaBudgetReached {
		lastError = message
	}
	catchup := !complete && !scopeChanged
	catchupScrolls := m.CatchupScrolls
	if !catchup {
		postAnchor, replyAnchor, catchupScrolls = "", "", 0
	}
	if refreshHead {
		catchupScrolls = 0
	}
	tx, err := a.store.DB.Begin()
	if err != nil {
		return "", id, err
	}
	defer tx.Rollback()
	if scopeChanged || (complete || refreshHead) && jobCtx.Err() == nil {
		if _, err = tx.Exec("DELETE FROM collection_progress WHERE target=?", m.Username); err != nil {
			return "", id, err
		}
	}
	continueHead := refreshHead && !scopeChanged && failure == nil && jobCtx.Err() == nil
	if continueHead {
		// Commit the new mode alongside cleared cursors and head anchors. A restart
		// must resume this same manual/automatic slot, not start another full rescan.
		if _, err = tx.Exec("UPDATE collection_jobs SET mode=?,status='queued',retry_at=0,queue_order=?,message=? WHERE id=? AND finished_at=0", collectionModeIncremental, time.Now().UnixNano(), message, jobID); err != nil {
			return "", id, err
		}
	}
	if rateUntil > 0 {
		if err = saveRateLimit(tx, rateUntil, rateFailures); err != nil {
			return "", id, err
		}
		if _, err = tx.Exec("UPDATE monitors SET next_run=CASE WHEN next_run<? THEN ? ELSE next_run END WHERE username=?", rateUntil, rateUntil, m.Username); err != nil {
			return "", id, err
		}
	}
	if archive.FailureKind(collectErr) == "browser" && m.Requested && jobCtx.Err() == nil {
		if _, err = tx.Exec("UPDATE monitors SET requested=1 WHERE username=?", m.Username); err != nil {
			return "", id, err
		}
	}
	if err = archive.EndRunTx(tx, id, status, result.Posts, message); err != nil {
		return "", id, err
	}
	archiveComplete := complete && failure == nil && remainingMedia == 0 && jobCtx.Err() == nil
	_, err = tx.Exec(`UPDATE monitors SET running=0,last_finished=?,last_success=CASE WHEN ? THEN ? ELSE last_success END,last_error=?,failures=?,last_posts=?,new_posts=?,catchup=?,catchup_post_anchor=?,catchup_reply_anchor=?,catchup_scroll_budget=?,baseline_status=?,posts_baseline_status=?,replies_baseline_status=?,baseline_finished=?,baseline_stop_reason=? WHERE username=?`, ended, archiveComplete, ended, lastError, failures, result.Posts, max(count-m.Posts, 0), catchup, postAnchor, replyAnchor, catchupScrolls, baselineStatus, postsBaseline, repliesBaseline, baselineFinished, baselineStop, m.Username)

	if err != nil {
		return "", id, err
	}
	if err = tx.Commit(); err != nil {
		return "", id, err
	}
	if rateUntil > 0 {
		log.Printf("collection rate limit target=%s failures=%d retry_at=%s detail=%s", m.Username, rateFailures, time.Unix(rateUntil, 0).UTC().Format(time.RFC3339), message)
	}
	kind := ""
	batchKind := ""
	if collectErr != nil {
		kind = archive.FailureKind(collectErr)
		if kind == "" {
			kind = "transient"
		}
		batchKind = kind
	} else if mediaErr != nil {
		kind = "media"
	}
	if kind == "auth" && state(a.store.DB, "cookie_imported") != cookieGeneration {
		kind = ""
		batchKind = "auth_replaced"
	}
	if continueHead {
		batchKind = "refresh_head"
	}
	err = a.recordNotifications(cfg, m, archiveComplete, jobCtx.Err() != nil, kind, failures)
	return batchKind, id, err
}

func retryJobStatus(kind string) string {
	switch kind {
	case "refresh_head":
		return "queued"
	case "rate":
		return "waiting_rate"
	case "auth", "auth_replaced":
		return "waiting_auth"
	default:
		return "retrying"
	}
}

func (s *Scheduler) queue(target string, modes ...string) error {
	mode := collectionModeIncremental
	if len(modes) > 0 {
		mode = modes[0]
	}
	_, err := s.queueAPI(target, mode, "")
	return err
}

func (s *Scheduler) queueAPI(target, requestedMode, credentialID string) (int64, error) {
	a := s.app
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	var err error
	if target, err = archive.Username(target); err != nil {
		return 0, err
	}
	mode := requestedMode
	if mode, err = normalizeCollectionMode(mode); err != nil {
		return 0, err
	}
	if _, err := a.store.LoadCookies(); err != nil {
		return 0, fmt.Errorf("请先导入有效格式的 Cookie")
	}
	if state(a.store.DB, "cookie_blocked") != "" {
		return 0, errors.New("登录状态受限，请先重新导入 Cookie")
	}
	tx, err := a.store.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var monitor Monitor
	if err = tx.QueryRow("SELECT baseline_status,replies,replies_baseline_status,running,requested FROM monitors WHERE username=?", target).Scan(&monitor.BaselineStatus, &monitor.Replies, &monitor.RepliesBaseline, &monitor.Running, &monitor.Requested); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errors.New("账号不存在")
		}
		return 0, err
	}
	if credentialID != "" {
		var active int
		if err = tx.QueryRow("SELECT COUNT(*) FROM collection_jobs WHERE target=? AND finished_at=0", target).Scan(&active); err != nil {
			return 0, err
		}
		if active > 0 {
			return 0, errors.New("账号已在队列或正在采集")
		}
	}
	if monitor.Running || monitor.Requested {
		return 0, errors.New("账号已在队列或正在采集")
	}
	if mode == collectionModeIncremental {
		mode = automaticCollectionMode(monitor)
	}
	if mode == collectionModeRescan {
		if err = resetMonitorBaseline(tx, target); err != nil {
			return 0, err
		}
	}
	r, err := tx.Exec("UPDATE monitors SET requested=1 WHERE username=? AND running=0 AND requested=0", target)
	if err != nil {
		return 0, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return 0, errors.New("账号不存在，或已在队列/采集中")
	}
	source := "manual"
	if credentialID != "" {
		source = "api"
	}
	jobID, err := enqueueCollectionJob(tx, target, source, mode, "手动请求")
	if err != nil {
		return 0, err
	}
	if credentialID != "" {
		if _, err = tx.Exec("UPDATE collection_jobs SET api_credential_id=?,requested_mode=? WHERE id=?", credentialID, requestedMode, jobID); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	s.Wake()
	return jobID, nil
}
