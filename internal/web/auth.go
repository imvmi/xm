package web

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"xm/internal/archive"
)

const iterations = 600000

type passwordRecord struct {
	Salt []byte `json:"salt"`
	Hash []byte `json:"hash"`
}
type session struct {
	CSRF    string
	Expires time.Time
}
type attempt struct {
	Count int
	Start time.Time
}
type auth struct {
	record   passwordRecord
	mu       sync.Mutex
	sessions map[[32]byte]session
	attempts map[string]attempt
	secure   bool
}

func randomToken() string { return base64.RawURLEncoding.EncodeToString(randomBytes(32)) }
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func newAuth(dir, password string, secure bool) (*auth, string, error) {
	a := &auth{sessions: map[[32]byte]session{}, attempts: map[string]attempt{}, secure: secure}
	path := filepath.Join(dir, "admin.json")
	b, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(b, &a.record) != nil || len(a.record.Hash) != 32 || len(a.record.Salt) != 32 {
			return nil, "", errors.New("管理员密码文件损坏")
		}
		return a, "", nil
	}
	if !os.IsNotExist(err) {
		return nil, "", err
	}
	generated := ""
	if password == "" {
		password = randomToken()
		generated = password
	}
	if len(password) < 12 || len(password) > 1024 {
		return nil, "", errors.New("管理员密码需为 12～1024 字节")
	}
	a.record.Salt = randomBytes(32)
	a.record.Hash, err = pbkdf2.Key(sha256.New, password, a.record.Salt, iterations, 32)
	if err != nil {
		return nil, "", err
	}
	b, err = json.Marshal(a.record)
	if err != nil {
		return nil, "", err
	}
	if err = archive.AtomicWrite(path, b); err != nil {
		return nil, "", err
	}
	return a, generated, nil
}
func (a *auth) session(r *http.Request) (session, bool) {
	c, err := r.Cookie("xm_session")
	if err != nil || len(c.Value) > 100 {
		return session{}, false
	}
	key := sha256.Sum256([]byte(c.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[key]
	if !ok {
		return s, false
	}
	if time.Now().After(s.Expires) {
		delete(a.sessions, key)
		return session{}, false
	}
	return s, true
}
func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.mu.Lock()
	for k, v := range a.attempts {
		if time.Since(v.Start) > time.Minute {
			delete(a.attempts, k)
		}
	}
	try := a.attempts[ip]
	if try.Start.IsZero() {
		try.Start = time.Now()
	}
	try.Count++
	a.attempts[ip] = try
	blocked := try.Count > 6 || len(a.attempts) > 1000
	a.mu.Unlock()
	if blocked {
		w.Header().Set("Retry-After", "60")
		problem(w, 429, "登录尝试过多，请一分钟后重试")
		return
	}
	if len(input.Password) > 1024 {
		problem(w, 401, "密码不正确")
		return
	}
	hash, err := pbkdf2.Key(sha256.New, input.Password, a.record.Salt, iterations, 32)
	if err != nil || subtle.ConstantTimeCompare(hash, a.record.Hash) != 1 {
		problem(w, 401, "密码不正确")
		return
	}
	token := randomToken()
	s := session{CSRF: randomToken(), Expires: time.Now().Add(12 * time.Hour)}
	a.mu.Lock()
	delete(a.attempts, ip)
	for k, v := range a.sessions {
		if time.Now().After(v.Expires) {
			delete(a.sessions, k)
		}
	}
	if len(a.sessions) >= 100 {
		a.sessions = map[[32]byte]session{}
	}
	a.sessions[sha256.Sum256([]byte(token))] = s
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "xm_session", Value: token, Path: "/", HttpOnly: true, Secure: a.secure || r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	respond(w, 200, map[string]string{"csrf": s.CSRF})
}
func (a *auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("xm_session"); err == nil {
		a.mu.Lock()
		delete(a.sessions, sha256.Sum256([]byte(c.Value)))
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "xm_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure || r.TLS != nil, SameSite: http.SameSiteStrictMode})
	respond(w, 200, map[string]bool{"ok": true})
}
