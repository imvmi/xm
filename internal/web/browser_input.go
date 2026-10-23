package web

import (
	"errors"
	"strings"

	"xm/internal/archive"
)

// Used by both saved settings and diagnostics. Diagnostics apply this only to
// a config snapshot, so testing a draft cannot alter running or future tasks.
type browserSettingsInput struct {
	BrowserMode *string `json:"browser_mode"`
	CDPHost     *string `json:"cdp_host"`
	CDPPort     *int    `json:"cdp_port"`
}

func (v browserSettingsInput) apply(cfg *archive.Config) error {
	if v.BrowserMode != nil {
		if *v.BrowserMode != "local" && *v.BrowserMode != "cdp" {
			return errors.New("浏览器模式必须为 local 或 cdp")
		}
		cfg.BrowserMode = *v.BrowserMode
	}
	if v.CDPHost != nil {
		cfg.CDPHost = strings.TrimSpace(*v.CDPHost)
	}
	if v.CDPPort != nil {
		cfg.CDPPort = *v.CDPPort
	}
	if (v.CDPHost != nil && cfg.CDPHost == "") || (v.CDPPort != nil && cfg.CDPPort < 1) {
		return errors.New("CDP 地址不能为空，端口必须为 1～65535")
	}
	return archive.ValidateBrowser(*cfg)
}
