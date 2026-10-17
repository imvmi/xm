package archive

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"xm/internal/database"
)

type Cookies struct {
	AuthToken string `json:"auth_token"`
	CSRF      string `json:"ct0"`
}

// ParseCookies accepts Cookie headers, browser JSON arrays, JSON maps, and Netscape files.
// Only the two required session cookies are retained. Foreign-domain cookies are ignored.
func ParseCookies(data []byte) (Cookies, error) {
	var c Cookies
	set := func(name, value string) {
		switch name {
		case "auth_token":
			c.AuthToken = value
		case "ct0":
			c.CSRF = value
		}
	}
	s := strings.TrimSpace(string(data))
	if strings.HasPrefix(s, "[") {
		var items []struct{ Name, Value, Domain string }
		if json.Unmarshal(data, &items) != nil {
			return c, errors.New("Cookie JSON 数组无效")
		}
		for _, v := range items {
			if v.Domain == "" || cookieDomain(v.Domain) {
				set(v.Name, v.Value)
			}
		}
	} else if strings.HasPrefix(s, "{") {
		var items map[string]string
		if json.Unmarshal(data, &items) != nil {
			return c, errors.New("Cookie JSON 应为名称到字符串值的映射")
		}
		for k, v := range items {
			set(k, v)
		}
	} else if strings.Contains(s, "\t") {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimPrefix(strings.TrimSpace(line), "#HttpOnly_")
			if strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) == 7 && cookieDomain(parts[0]) {
				set(parts[5], parts[6])
			}
		}
	} else {
		if len(s) >= 7 && strings.EqualFold(s[:7], "cookie:") {
			s = strings.TrimSpace(s[7:])
		}
		r := &http.Request{Header: http.Header{"Cookie": []string{s}}}
		for _, v := range r.Cookies() {
			set(v.Name, v.Value)
		}
	}
	for _, v := range []string{c.AuthToken, c.CSRF} {
		if len(v) == 0 || len(v) > 4096 {
			return Cookies{}, errors.New("Cookie 缺少 auth_token 或 ct0，或值长度不正确")
		}
		for _, ch := range v {
			if ch < 33 || ch > 126 || strings.ContainsRune(";,\"\\", ch) {
				return Cookies{}, errors.New("Cookie 值包含无效字符")
			}
		}
	}
	return c, nil
}

func cookieDomain(s string) bool {
	s = strings.ToLower(strings.TrimPrefix(s, "."))
	return s == "x.com" || s == "twitter.com" || s == "www.x.com" || s == "www.twitter.com"
}

// SaveCookies stores the X session directly in the configured database.
// The singleton row is replaced atomically; credentials never enter config JSON.
func (s *Store) SaveCookies(c Cookies) error {
	plain, err := json.Marshal(c)
	if err != nil {
		return errors.New("Cookie 格式无效")
	}
	if _, err = ParseCookies(plain); err != nil {
		return err
	}
	_, err = s.DB.Exec(database.Upsert(s.DB.Dialect,
		"INSERT INTO x_cookies(id,auth_token,ct0,updated_at) VALUES(1,?,?,?)",
		[]string{"id"}, []string{"auth_token", "ct0", "updated_at"}), c.AuthToken, c.CSRF, now())
	if err != nil {
		return errors.New("保存 Cookie 到数据库失败")
	}
	return nil
}

func (s *Store) LoadCookies() (Cookies, error) {
	var c Cookies
	err := s.DB.QueryRow("SELECT auth_token,ct0 FROM x_cookies WHERE id=1").Scan(&c.AuthToken, &c.CSRF)
	if errors.Is(err, sql.ErrNoRows) {
		return Cookies{}, errors.New("请先导入 Cookie")
	}
	if err != nil {
		return Cookies{}, errors.New("读取数据库 Cookie 失败")
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return Cookies{}, errors.New("数据库 Cookie 格式无效")
	}
	return ParseCookies(plain)
}
