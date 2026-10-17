package web

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"
	"xm/internal/archive"
)

const iterations = 600000

type passwordRecord = archive.AdminCredentials

var adminUsername = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{2,31}$`)

func makePasswordRecord(username, password string) (passwordRecord, error) {
	if !adminUsername.MatchString(username) {
		return passwordRecord{}, errors.New("用户名需为 3～32 位字母、数字、下划线、点或短横线，首位为字母、数字或下划线")
	}
	if len(password) < 8 || len(password) > 1024 {
		return passwordRecord{}, errors.New("管理员密码需为 8～1024 字节")
	}
	r := passwordRecord{Username: username, Salt: randomBytes(32), ConfigurationLocked: true}
	var err error
	r.Hash, err = pbkdf2.Key(sha256.New, password, r.Salt, iterations, 32)
	return r, err
}

func verifyPassword(record passwordRecord, username, password string) bool {
	if len(password) > 1024 {
		return false
	}
	hash, err := pbkdf2.Key(sha256.New, password, record.Salt, iterations, 32)
	return err == nil && subtle.ConstantTimeCompare(hash, record.Hash) == 1 && username == record.Username
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
func loadAdmin(s *archive.Store) (passwordRecord, bool, error) {
	record, err := s.AdminCredentials()
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, errors.New("无法读取数据库管理员凭据")
	}
	if !record.ConfigurationLocked || !adminUsername.MatchString(record.Username) || len(record.Salt) != 32 || len(record.Hash) != 32 {
		return record, false, errors.New("数据库管理员凭据损坏，请恢复备份")
	}
	return record, true, nil
}
func installationReady(cfg archive.Config) (bool, error) {
	s, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		return false, err
	}
	defer s.Close()
	_, ready, err := loadAdmin(s)
	if err != nil || !ready {
		return ready, err
	}
	_, err = s.LoadSettings(cfg)
	return ready, err
}
func newAuth(s *archive.Store, secure bool) (*auth, error) {
	record, ready, err := loadAdmin(s)
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, errors.New("请先运行 xm serve 完成首次设置")
	}
	return &auth{record: record, sessions: map[[32]byte]session{}, attempts: map[string]attempt{}, secure: secure}, nil
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
		Username string `json:"username"`
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
	if !verifyPassword(a.record, input.Username, input.Password) {
		problem(w, 401, "用户名或密码不正确")
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
