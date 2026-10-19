package archive

import (
	"bytes"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
)

type Config struct {
	Database             DatabaseConfig        `json:"database,omitempty"`
	NotificationChannels []NotificationChannel `json:"notification_channels,omitempty"`
	MediaStorage         string                `json:"media_storage,omitempty"`
	StorageProfiles      []StorageProfile      `json:"storage_profiles,omitempty"`
	DataDir              string                `json:"data_dir"`
	Proxy                string                `json:"proxy"`
	ChromePath           string                `json:"chrome_path"`
	Headless             bool                  `json:"headless"`
	RequestTimeout       int                   `json:"request_timeout_seconds"`
	RequestInterval      int                   `json:"request_interval_seconds"`
	RequestJitter        int                   `json:"request_jitter_seconds"`
	RequestBudgetMode    string                `json:"request_budget_mode"` // Legacy persisted value; request-count limits are no longer enforced.
	RateLimitMode        string                `json:"rate_limit_mode"`
	CollectionTimeout    int                   `json:"collection_timeout_seconds"`
	ScrollDelay          int                   `json:"scroll_delay_seconds"`
	ScrollJitter         int                   `json:"scroll_jitter_seconds"`
	MaxScrolls           int                   `json:"max_scrolls"`
	MaxPostsPerRun       int                   `json:"max_posts_per_run"`
	PageBudget           int                   `json:"page_budget"`
	TimelineIdleSeconds  int                   `json:"timeline_idle_seconds"`
	KnownBoundaryPosts   int                   `json:"known_boundary_posts"`
	MaxMediaBytes        int64                 `json:"max_media_bytes"`
	BrowserMaxAge        int                   `json:"browser_max_age_seconds"`
	BrowserMaxTasks      int                   `json:"browser_max_tasks"`
	MediaRetryInitial    int                   `json:"media_retry_initial_seconds"`
	MediaRetryMax        int                   `json:"media_retry_max_seconds"`
	QueueInterval        int                   `json:"queue_interval_seconds"`
	DefaultInterval      int                   `json:"default_interval_seconds"`
	PollingPaused        bool                  `json:"polling_paused"`
}

// Bootstrap is the only configuration kept outside the database.
// DataDir locates SQLite/local media and the worker lock; Database locates MySQL.
type Bootstrap struct {
	DataDir  string         `yaml:"data_dir"`
	Database DatabaseConfig `yaml:"database"`
}

func DefaultConfig() Config {
	return Config{DataDir: "data", Headless: true, RequestTimeout: 60, RequestInterval: 5, RequestBudgetMode: "balanced", RateLimitMode: "balanced", CollectionTimeout: 300, ScrollDelay: 5, ScrollJitter: 2, MaxScrolls: 40, MaxPostsPerRun: 1000, PageBudget: 15, TimelineIdleSeconds: 24, KnownBoundaryPosts: 8, MaxMediaBytes: 512 << 20, BrowserMaxAge: 6 * 3600, BrowserMaxTasks: 100, MediaRetryInitial: 5 * 60, MediaRetryMax: 24 * 3600, QueueInterval: 0, DefaultInterval: 3600}
}
func LoadBootstrap(path string) (Config, error) {
	c := DefaultConfig()
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	b := &Bootstrap{DataDir: c.DataDir}
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if err = d.Decode(&b); err != nil || b == nil {
		return c, errors.New("启动配置 YAML 无效，仅支持 data_dir 和 database")
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, errors.New("配置文件只能包含一个 YAML 文档")
	}
	c.DataDir = b.DataDir
	c.Database = b.Database
	return normalizeConfig(path, c)
}
func LoadConfig(path string) (Config, error) {
	c, err := LoadBootstrap(path)
	if err != nil {
		return c, err
	}
	s, err := OpenConfiguredStore(c)
	if err != nil {
		return c, err
	}
	defer s.Close()
	return s.LoadSettings(c)
}
func normalizeConfig(path string, c Config) (Config, error) {
	var err error
	defaults := DefaultConfig()
	if c.MaxPostsPerRun == 0 {
		c.MaxPostsPerRun = defaults.MaxPostsPerRun
	}
	if c.PageBudget == 0 {
		c.PageBudget = defaults.PageBudget
	}
	if c.TimelineIdleSeconds == 0 {
		c.TimelineIdleSeconds = defaults.TimelineIdleSeconds
	}
	if c.KnownBoundaryPosts == 0 {
		c.KnownBoundaryPosts = defaults.KnownBoundaryPosts
	}
	if c.RequestBudgetMode == "" {
		c.RequestBudgetMode = defaults.RequestBudgetMode
	}
	if c.RateLimitMode == "" {
		c.RateLimitMode = defaults.RateLimitMode
	}
	if c.BrowserMaxAge == 0 {
		c.BrowserMaxAge = defaults.BrowserMaxAge
	}
	if c.BrowserMaxTasks == 0 {
		c.BrowserMaxTasks = defaults.BrowserMaxTasks
	}
	if c.MediaRetryInitial == 0 {
		c.MediaRetryInitial = defaults.MediaRetryInitial
	}
	if c.MediaRetryMax == 0 {
		c.MediaRetryMax = defaults.MediaRetryMax
	}
	if c.DataDir == "" || c.RequestTimeout < 1 || c.CollectionTimeout < 1 || c.ScrollDelay < 1 || c.MaxScrolls < 1 || c.MaxMediaBytes < 1 || c.MaxMediaBytes > 10<<30 {
		return c, errors.New("配置中的目录不能为空，超时/分页/大小限制必须为正数；媒体上限不超过 10 GiB")
	}
	if c.CollectionTimeout > 3600 || c.ScrollDelay > 60 || c.MaxScrolls > 1000 {
		return c, errors.New("采集时长、滚动间隔或滚动次数超过系统安全上限")
	}
	if c.ScrollJitter < 0 || c.ScrollJitter >= c.ScrollDelay {
		return c, errors.New("滚动随机浮动必须大于等于 0，且小于滚动基础等待")
	}
	if c.MaxPostsPerRun < 1 || c.MaxPostsPerRun > 10000 || c.PageBudget < 1 || c.PageBudget > 100 || c.TimelineIdleSeconds < 1 || c.TimelineIdleSeconds > 300 || c.KnownBoundaryPosts < 1 || c.KnownBoundaryPosts > 100 {
		return c, errors.New("高级采集设置超出系统安全范围")
	}
	if !validRateLimitMode(c.RateLimitMode) || c.BrowserMaxAge < 300 || c.BrowserMaxAge > 86400 || c.BrowserMaxTasks < 1 || c.BrowserMaxTasks > 1000 || c.MediaRetryInitial < 60 || c.MediaRetryInitial > 86400 || c.MediaRetryMax < c.MediaRetryInitial || c.MediaRetryMax > 604800 {
		return c, errors.New("请求、浏览器或媒体重试设置超出系统安全范围")
	}
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(filepath.Dir(path), c.DataDir)
	}
	if err := c.Database.resolvePaths(filepath.Dir(path)); err != nil {
		return c, err
	}
	if err = c.Database.Validate(); err != nil {
		return c, err
	}
	c.DataDir, err = filepath.Abs(c.DataDir)
	if err != nil {
		return c, err
	}
	if _, err = proxyURL(c.Proxy); err != nil {
		return c, err
	}
	if c.DefaultInterval == 0 {
		c.DefaultInterval = 3600
	}
	if c.DefaultInterval < 3600 || c.DefaultInterval > 604800 {
		return c, errors.New("自动采集周期必须为 1～168 小时")
	}
	if c.QueueInterval < 0 || c.QueueInterval > 3600 {
		return c, errors.New("队列任务间隔必须为 0～3600 秒")
	}
	if c.RequestInterval == 0 {
		c.RequestInterval = 5
	}
	if err = ValidateRequestInterval(c.RequestInterval); err != nil {
		return c, err
	}
	if err = ValidateRequestJitter(c.RequestInterval, c.RequestJitter); err != nil {
		return c, err
	}
	if err = c.ValidateStorage(); err != nil {
		return c, err
	}
	return c, c.ValidateNotifications()
}

func validRateLimitMode(mode string) bool {
	return mode == "conservative" || mode == "balanced" || mode == "fast"
}

// ValidateCollectionTuning applies the conservative ranges exposed in the UI.
// normalizeConfig keeps slightly wider recovery ceilings for existing data.
func ValidateCollectionTuning(c Config) error {
	if c.ScrollDelay < 1 || c.ScrollDelay > 30 {
		return errors.New("滚动基础等待必须为 1～30 秒")
	}
	if c.ScrollJitter < 0 || c.ScrollJitter >= c.ScrollDelay {
		return errors.New("滚动随机浮动必须大于等于 0，且小于滚动基础等待")
	}
	if c.TimelineIdleSeconds < 10 || c.TimelineIdleSeconds > 120 {
		return errors.New("时间线无进展结束等待必须为 10～120 秒")
	}
	if c.KnownBoundaryPosts < 3 || c.KnownBoundaryPosts > 50 {
		return errors.New("已归档推文停止阈值必须为 3～50 条")
	}
	if c.CollectionTimeout < 60 || c.CollectionTimeout > 1800 {
		return errors.New("单轮采集时长必须为 60～1800 秒")
	}
	return nil
}

func ValidateOperationalTuning(c Config) error {
	if c.RequestTimeout < 10 || c.RequestTimeout > 300 {
		return errors.New("网络请求超时必须为 10～300 秒")
	}
	if c.MaxMediaBytes < 1<<20 || c.MaxMediaBytes > 10<<30 {
		return errors.New("单媒体文件上限必须为 1 MiB～10 GiB")
	}
	if !validRateLimitMode(c.RateLimitMode) {
		return errors.New("限流冷却预设无效")
	}
	if c.BrowserMaxAge < 3600 || c.BrowserMaxAge > 86400 {
		return errors.New("浏览器最长复用时间必须为 1～24 小时")
	}
	if c.BrowserMaxTasks < 10 || c.BrowserMaxTasks > 1000 {
		return errors.New("单浏览器任务数必须为 10～1000")
	}
	if c.MediaRetryInitial < 60 || c.MediaRetryInitial > 86400 {
		return errors.New("媒体首次重试间隔必须为 1 分钟～24 小时")
	}
	if c.MediaRetryMax < c.MediaRetryInitial || c.MediaRetryMax > 604800 {
		return errors.New("媒体最长重试间隔必须不小于首次间隔，且不超过 7 天")
	}
	return nil
}

func ValidateRequestInterval(seconds int) error {
	if seconds < 5 {
		return errors.New("请求间隔必须为不少于 5 秒的整数")
	}
	// Match the database INTEGER range and keep duration conversion safe.
	if seconds > 2147483647 {
		return errors.New("请求间隔过大")
	}
	return nil
}

func ValidateRequestJitter(interval, jitter int) error {
	if jitter < 0 {
		return errors.New("请求随机浮动不能小于 0 秒")
	}
	if jitter > interval-5 {
		return errors.New("请求随机浮动过大，间隔下限不能低于 5 秒")
	}
	return nil
}

// SaveBootstrap writes only the information needed to locate the database.
func SaveBootstrap(path string, c Config) error {
	if c.DataDir == "" {
		return errors.New("数据目录不能为空")
	}
	if err := c.Database.Validate(); err != nil {
		return err
	}
	b, err := MarshalBootstrap(Bootstrap{DataDir: c.DataDir, Database: c.Database})
	if err != nil {
		return err
	}
	return AtomicWrite(path, b)
}

func MarshalBootstrap(b Bootstrap) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("# XM 自动生成的启动配置；其他设置保存在数据库中。\n")
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(b); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func PrivateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

// AtomicWrite never leaves half-written credentials or metadata behind.
func AtomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("保存文件: %w", err)
	}
	return nil
}
