package web

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"xm/internal/archive"
)

type identityLookupFailure struct {
	collectionErr    error
	cookieGeneration string
}

// The running job owns the serial scheduler. Recover once before any timeline
// data is saved, and retry only when the numeric ID proves an actual rename.
func (s *Scheduler) recoverRenamedAccount(ctx context.Context, cfg archive.Config, cookies archive.Cookies, generation, target string, identity archive.TargetIdentity, original error) (archive.TargetIdentity, bool, error) {
	a := s.app
	gates, err := loadSchedulerState(a.store.DB)
	if err != nil {
		return identity, false, err
	}
	if gates.CookieGeneration != generation || gates.CookieBlocked != "" || gates.CooldownUntil > time.Now().Unix() {
		return identity, false, original
	}
	refs, err := a.store.IdentityReferencePosts(target, identity.UserID)
	if err != nil {
		return identity, false, err
	}
	s.setPhase("identity")
	// A separate identity lookup must not share the collector's live CDP tab.
	s.closeSession()
	lookupCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	profile, err := a.refreshIdentity(lookupCtx, cfg, cookies, identity, refs, "")
	if ctx.Err() != nil {
		return identity, false, ctx.Err()
	}
	if err != nil {
		// Preserve global stop signals and newly discovered explicit diagnoses.
		if archive.FailureKind(err) == "auth" || archive.FailureKind(err) == "rate" || archive.FailureKind(err) == "identity" || archive.FailureReason(err) == "suspended" || archive.FailureReason(err) == "protected" {
			return identity, false, err
		}
		return identity, false, &archive.Failure{Kind: "access", Reason: archive.FailureReason(original), Message: original.Error() + "；自动更新账号身份未成功：" + safeMessage(err)}
	}
	if lookupCtx.Err() != nil {
		return identity, false, &archive.Failure{Kind: "access", Message: original.Error() + "；自动更新账号身份超时"}
	}
	if profile.ID != identity.UserID {
		return identity, false, &archive.Failure{Kind: "identity", Message: "账号身份不一致：数字用户 ID 与原账号不符，未更新"}
	}
	if strings.EqualFold(profile.Username, identity.CurrentUsername) {
		return identity, false, &archive.Failure{Kind: "access", Message: original.Error() + "；自动核对后用户名未变化，未确认改名"}
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	if ctx.Err() != nil {
		return identity, false, ctx.Err()
	}
	if err = a.store.SaveTargetProfile(target, profile); err != nil {
		return identity, false, err
	}
	identity.CurrentUsername = strings.ToLower(profile.Username)
	return identity, true, nil
}

// Reserve the serial collector while resolving identity. Never share its live
// browser session with an HTTP handler, or let two lookups bypass pacing.
func (a *App) reserveIdentityRefresh() bool {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	a.scheduler.mu.Lock()
	defer a.scheduler.mu.Unlock()
	if a.scheduler.active || a.scheduler.recovery != nil || a.identityFailure != nil {
		return false
	}
	a.scheduler.active = true
	return true
}

func (a *App) updateMonitorIdentity(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decode(w, r, &input) {
		return
	}
	target, err := archive.Username(r.PathValue("username"))
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if input.Username != "" {
		input.Username, err = archive.Username(input.Username)
		if err != nil {
			problem(w, 400, err.Error())
			return
		}
	}
	if !a.reserveIdentityRefresh() {
		problem(w, 409, "正在采集或更新账号身份，请等待当前任务结束后重试")
		return
	}
	defer func() {
		a.scheduler.mu.Lock()
		a.scheduler.active = false
		a.scheduler.mu.Unlock()
		a.scheduler.Wake()
	}()
	var running bool
	if err = a.store.DB.QueryRow("SELECT running FROM monitors WHERE username=?", target).Scan(&running); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			problem(w, 404, "监控账号不存在")
		} else {
			problem(w, 500, "读取监控账号失败")
		}
		return
	}
	if running {
		problem(w, 409, "该账号正在采集，请稍后重试")
		return
	}
	a.changeMu.Lock()
	gates, err := loadSchedulerState(a.store.DB)
	cookies, cookieErr := a.store.LoadCookies()
	cfg := a.Config()
	a.changeMu.Unlock()
	if err != nil {
		problem(w, 500, err.Error())
		return
	}
	if gates.CookieBlocked != "" {
		problem(w, 409, "Cookie 登录态失效，请先重新导入 Cookie")
		return
	}
	if gates.CooldownUntil > time.Now().Unix() {
		problem(w, 429, "X 请求限流冷却中，请在冷却结束后重试")
		return
	}
	identity, err := a.store.TargetIdentity(target)
	if err != nil {
		problem(w, 409, err.Error())
		return
	}
	if identity.UserID == "" {
		problem(w, 409, "尚未保存可核对的数字用户 ID，无法确认新用户名属于原账号")
		return
	}
	references, err := a.store.IdentityReferencePosts(target, identity.UserID)
	if err != nil {
		problem(w, 500, "读取历史推文失败")
		return
	}
	if cookieErr != nil {
		problem(w, 409, "请先导入可用的 Cookie")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	profile, lookupErr := a.refreshIdentity(ctx, cfg, cookies, identity, references, input.Username)
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	if lookupErr != nil {
		// Persist upstream stop signals before allowing the scheduler to continue.
		if err = a.recordIdentityLookupFailure(lookupErr, gates.CookieGeneration); err != nil {
			a.identityFailure = &identityLookupFailure{collectionErr: lookupErr, cookieGeneration: gates.CookieGeneration}
			problem(w, 500, "保存身份查询状态失败，已暂停后续查询与采集，等待数据库恢复")
			return
		}
		status := http.StatusBadGateway
		message := lookupErr.Error()
		if archive.FailureKind(lookupErr) == "rate" {
			status = http.StatusTooManyRequests
		}
		if errors.Is(lookupErr, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			message = "身份查找超时，请稍后重试，或填写当前用户名核对"
		}
		problem(w, status, message)
		return
	}
	if ctx.Err() != nil {
		problem(w, 408, "身份更新已取消或超时，未保存")
		return
	}
	// The user may have removed this monitor while the browser was looking it up.
	var enabled bool
	if err = a.store.DB.QueryRow("SELECT enabled FROM monitors WHERE username=?", target).Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			problem(w, 404, "监控账号已移除，未更新")
		} else {
			problem(w, 500, "读取监控账号失败，未更新")
		}
		return
	}
	if profile.ID != identity.UserID {
		problem(w, 409, "账号身份不一致：数字用户 ID 与原账号不符，未更新")
		return
	}
	if err = a.store.SaveTargetProfile(target, profile); err != nil {
		problem(w, 409, err.Error())
		return
	}
	message := "已更新为 @" + profile.Username
	if strings.EqualFold(identity.CurrentUsername, profile.Username) {
		message = "已确认当前账号为 @" + profile.Username + "，用户名未变化"
	}
	if !enabled {
		message += "；自动采集仍暂停，可在账号卡片中开启"
	}
	respond(w, 200, map[string]any{"user_id": profile.ID, "current_username": profile.Username, "previous_username": identity.CurrentUsername, "enabled": enabled, "message": message})
}

// Caller holds changeMu. A stale login failure must not block newly imported cookies.
func (a *App) recordIdentityLookupFailure(failure error, generation string) error {
	kind := archive.FailureKind(failure)
	if kind != "auth" && kind != "rate" {
		return nil
	}
	gates, err := loadSchedulerState(a.store.DB)
	if err != nil {
		return err
	}
	if kind == "auth" {
		if gates.CookieGeneration == generation {
			return setState(a.store.DB, "cookie_blocked", failure.Error())
		}
		return nil
	}
	cfg := a.Config()
	until, count := rateLimitDeadline(time.Now().Unix(), cfg.DefaultInterval, gates.RateFailures, failure, cfg.RateLimitMode)
	tx, err := a.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveRateLimit(tx, max(until, gates.CooldownUntil), count); err != nil {
		return err
	}
	return tx.Commit()
}
