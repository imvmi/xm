package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"xm/internal/archive"
	"xm/internal/database"
)

func (a *App) cookieSource(w http.ResponseWriter, r *http.Request) {
	c, err := a.store.LoadCookieSource()
	if err != nil {
		problem(w, 500, err.Error())
		return
	}
	respond(w, 200, map[string]any{"url": c.URL, "token_configured": c.BearerToken != "", "auto_refresh": c.AutoRefresh, "use_proxy": c.UseProxy, "last_error": state(a.store.DB, "cookie_api_error"), "last_fetched": state(a.store.DB, "cookie_api_fetched")})
}

func (a *App) saveCookieSource(w http.ResponseWriter, r *http.Request) {
	var v struct {
		URL         string `json:"url"`
		Token       string `json:"token"`
		ClearToken  bool   `json:"clear_token"`
		AutoRefresh bool   `json:"auto_refresh"`
		UseProxy    bool   `json:"use_proxy"`
	}
	if !decode(w, r, &v) {
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	c, err := a.store.LoadCookieSource()
	if err != nil {
		problem(w, 500, err.Error())
		return
	}
	endpoint := strings.TrimSpace(v.URL)
	// Never silently send an existing credential to a newly entered destination.
	if endpoint != c.URL || v.ClearToken {
		c.BearerToken = ""
	}
	if v.Token != "" {
		c.BearerToken = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v.Token), "Bearer "))
	}
	c.URL = endpoint
	c.AutoRefresh = v.AutoRefresh
	c.UseProxy = v.UseProxy
	if err = c.Validate(); err != nil {
		problem(w, 400, err.Error())
		return
	}
	if err = a.store.SaveCookieSource(c); err != nil {
		problem(w, 500, err.Error())
		return
	}
	respond(w, 200, map[string]string{"message": "Cookie API 配置已保存到数据库"})
}

// Caller holds changeMu. Cookie replacement and clearing the blocked state must
// commit together, so a crash cannot leave a new credential blocked by old state.
func (a *App) saveImportedCookies(c archive.Cookies, resetRetry, fromAPI bool) error {
	tx, err := a.store.DB.Begin()
	if err != nil {
		return errors.New("保存 Cookie 失败")
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO x_cookies(id,auth_token,ct0,updated_at) VALUES(1,?,?,?)", []string{"id"}, []string{"auth_token", "ct0", "updated_at"}), c.AuthToken, c.CSRF, now)
	if err != nil {
		return errors.New("保存 Cookie 失败")
	}
	states := map[string]string{"cookie_blocked": "", "cookie_imported": now, "cookie_api_error": ""}
	if fromAPI {
		states["cookie_api_fetched"] = now
	}
	if resetRetry {
		states["cookie_api_attempted"] = ""
	}
	for k, v := range states {
		if _, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), k, v); err != nil {
			return errors.New("更新 Cookie 状态失败")
		}
	}
	if err = tx.Commit(); err != nil {
		return errors.New("保存 Cookie 失败")
	}
	return nil
}

// Caller holds changeMu to serialize imports and source changes with the fetch.
func (a *App) fetchAndSaveCookie(ctx context.Context, c archive.CookieSource, manual bool) error {
	cookies, err := archive.FetchCookies(ctx, c, a.Config().Proxy)
	if err == nil {
		err = a.saveImportedCookies(cookies, manual, true)
	}
	if err != nil {
		_ = setState(a.store.DB, "cookie_api_error", err.Error())
		return err
	}
	return nil
}

func (a *App) fetchCookie(w http.ResponseWriter, r *http.Request) {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	c, err := a.store.LoadCookieSource()
	if err != nil {
		problem(w, 500, err.Error())
		return
	}
	if c.URL == "" {
		problem(w, 400, "请先保存 Cookie API 配置")
		return
	}
	if err = a.fetchAndSaveCookie(r.Context(), c, true); err != nil {
		problem(w, 502, err.Error())
		return
	}
	a.scheduler.Wake()
	respond(w, 200, map[string]string{"message": "已从 API 获取并保存 Cookie，下一个采集任务生效"})
}

// One automatic attempt per authentication failure episode, persisted across
// restarts. Only successful authenticated collection or manual import rearms it.
func (a *App) recoverCookie(ctx context.Context, generation, target string) error {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	if ctx.Err() != nil || state(a.store.DB, "cookie_imported") != generation || state(a.store.DB, "cookie_api_attempted") == "1" {
		return nil
	}
	c, err := a.store.LoadCookieSource()
	if err != nil {
		return err
	}
	if c.URL == "" || !c.AutoRefresh {
		return nil
	}
	if err = setState(a.store.DB, "cookie_api_attempted", "1"); err != nil {
		return err
	}
	if err = a.fetchAndSaveCookie(ctx, c, false); err != nil {
		return err
	}
	// The current browser session must close before the retry uses new cookies.
	_, err = a.store.DB.Exec("UPDATE monitors SET requested=1 WHERE username=?", target)
	return err
}
