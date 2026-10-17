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
	CollectionTimeout    int                   `json:"collection_timeout_seconds"`
	ScrollDelay          int                   `json:"scroll_delay_seconds"`
	MaxScrolls           int                   `json:"max_scrolls"`
	MaxMediaBytes        int64                 `json:"max_media_bytes"`
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
	return Config{DataDir: "data", Headless: true, RequestTimeout: 60, CollectionTimeout: 300, ScrollDelay: 3, MaxScrolls: 40, MaxMediaBytes: 512 << 20, DefaultInterval: 900}
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
	if c.DataDir == "" || c.RequestTimeout < 1 || c.CollectionTimeout < 1 || c.ScrollDelay < 1 || c.MaxScrolls < 1 || c.MaxMediaBytes < 1 || c.MaxMediaBytes > 10<<30 {
		return c, errors.New("配置中的目录不能为空，超时/分页/大小限制必须为正数；媒体上限不超过 10 GiB")
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
		c.DefaultInterval = 900
	}
	if c.DefaultInterval < 60 || c.DefaultInterval > 604800 {
		return c, errors.New("默认轮询周期必须为 1 分钟到 7 天")
	}
	if err = c.ValidateStorage(); err != nil {
		return c, err
	}
	return c, c.ValidateNotifications()
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
