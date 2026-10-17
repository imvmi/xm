package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Config struct {
	DataDir           string `json:"data_dir"`
	Proxy             string `json:"proxy"`
	ChromePath        string `json:"chrome_path"`
	Headless          bool   `json:"headless"`
	RequestTimeout    int    `json:"request_timeout_seconds"`
	CollectionTimeout int    `json:"collection_timeout_seconds"`
	ScrollDelay       int    `json:"scroll_delay_seconds"`
	MaxScrolls        int    `json:"max_scrolls"`
	MaxMediaBytes     int64  `json:"max_media_bytes"`
	DefaultInterval   int    `json:"default_interval_seconds"`
	PollingPaused     bool   `json:"polling_paused"`
}

func LoadConfig(path string) (Config, error) {
	c := Config{DataDir: "data", Headless: true, RequestTimeout: 60, CollectionTimeout: 300, ScrollDelay: 3, MaxScrolls: 40, MaxMediaBytes: 512 << 20, DefaultInterval: 900}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, errors.New("配置 JSON 无效，或包含未知字段")
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, errors.New("配置文件只能包含一个 JSON 对象")
	}
	if c.DataDir == "" || c.RequestTimeout < 1 || c.CollectionTimeout < 1 || c.ScrollDelay < 1 || c.MaxScrolls < 1 || c.MaxMediaBytes < 1 || c.MaxMediaBytes > 10<<30 {
		return c, errors.New("配置中的目录不能为空，超时/分页/大小限制必须为正数；媒体上限不超过 10 GiB")
	}
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(filepath.Dir(path), c.DataDir)
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
	return c, nil
}

func SaveConfig(path string, c Config) error {
	// data_dir is absolute after loading, so moving the working directory will
	// not silently redirect the archive after a settings update.
	if _, err := proxyURL(c.Proxy); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(path, append(b, '\n'))
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
