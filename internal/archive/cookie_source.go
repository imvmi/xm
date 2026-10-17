package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"xm/internal/database"
)

// CookieSource is stored only in the database. Never serialize its credential.
type CookieSource struct {
	URL         string `json:"url"`
	BearerToken string `json:"-"`
	AutoRefresh bool   `json:"auto_refresh"`
	UseProxy    bool   `json:"use_proxy"`
}

func (c CookieSource) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || len(c.URL) > 4096 || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return errors.New("请输入完整的 HTTP / HTTPS 接口地址，不要包含用户名、密码或片段")
	}
	if len(c.BearerToken) > 4096 {
		return errors.New("Bearer Token 过长")
	}
	for _, ch := range c.BearerToken {
		if ch < 33 || ch > 126 {
			return errors.New("Bearer Token 不能包含空白或控制字符")
		}
	}
	return nil
}

func (s *Store) LoadCookieSource() (CookieSource, error) {
	var c CookieSource
	err := s.DB.QueryRow("SELECT endpoint,bearer_token,auto_refresh,use_proxy FROM cookie_sources WHERE id=1").Scan(&c.URL, &c.BearerToken, &c.AutoRefresh, &c.UseProxy)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, errors.New("读取 Cookie API 配置失败")
	}
	return c, nil
}

func (s *Store) SaveCookieSource(c CookieSource) error {
	if err := c.Validate(); err != nil {
		return err
	}
	_, err := s.DB.Exec(database.Upsert(s.DB.Dialect, "INSERT INTO cookie_sources(id,endpoint,bearer_token,auto_refresh,use_proxy,updated_at) VALUES(1,?,?,?,?,?)", []string{"id"}, []string{"endpoint", "bearer_token", "auto_refresh", "use_proxy", "updated_at"}), c.URL, c.BearerToken, c.AutoRefresh, c.UseProxy, now())
	if err != nil {
		return errors.New("保存 Cookie API 配置失败")
	}
	return nil
}

// FetchCookies never follows redirects or returns upstream bodies/URLs in errors.
func FetchCookies(ctx context.Context, source CookieSource, proxy string) (Cookies, error) {
	if err := source.Validate(); err != nil {
		return Cookies{}, err
	}
	if !source.UseProxy {
		proxy = ""
	}
	client, err := HTTPClient(proxy, 15*time.Second)
	if err != nil {
		return Cookies{}, errors.New("Cookie API 代理配置无效")
	}
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return Cookies{}, errors.New("Cookie API 请求地址无效")
	}
	req.Header.Set("Accept", "application/json")
	if source.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+source.BearerToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Cookies{}, NetworkError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Cookies{}, fmt.Errorf("Cookie API 返回 HTTP %d；未更新 Cookie", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return Cookies{}, errors.New("读取 Cookie API 响应失败")
	}
	if len(data) > 1<<20 {
		return Cookies{}, errors.New("Cookie API 响应超过 1 MiB")
	}
	if !json.Valid(data) || !strings.ContainsAny(strings.TrimSpace(string(data))[:1], "{[") {
		return Cookies{}, errors.New("Cookie API 应返回 JSON 对象或 Cookie 数组")
	}
	return ParseCookies(data)
}
