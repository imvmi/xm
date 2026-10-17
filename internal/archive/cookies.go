package archive

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

func vaultCipher(dir string, create bool) (cipher.AEAD, error) {
	path := filepath.Join(dir, "cookie.key")
	key, err := os.ReadFile(path)
	if os.IsNotExist(err) && create {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return nil, errors.New("无法读取 Cookie 密钥；请先导入 Cookie，恢复备份时同时恢复 cookie.key")
	}
	if len(key) != 32 {
		return nil, errors.New("Cookie 密钥文件无效")
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func SaveCookies(dir string, c Cookies) error {
	if err := PrivateDir(dir); err != nil {
		return err
	}
	a, err := vaultCipher(dir, true)
	if err != nil {
		return err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sealed := a.Seal(nonce, nonce, plain, []byte("xm-cookies-v1"))
	return AtomicWrite(filepath.Join(dir, "cookies.enc"), sealed)
}

func LoadCookies(dir string) (Cookies, error) {
	var c Cookies
	a, err := vaultCipher(dir, false)
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "cookies.enc"))
	if err != nil {
		return c, errors.New("请先运行 import-cookie 导入 Cookie")
	}
	if len(b) < a.NonceSize() {
		return c, errors.New("Cookie 文件损坏")
	}
	plain, err := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte("xm-cookies-v1"))
	if err != nil {
		return c, errors.New("Cookie 解密失败，文件或密钥不匹配")
	}
	return ParseCookies(plain)
}
