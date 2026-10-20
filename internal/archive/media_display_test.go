package archive

import "testing"

func TestMediaDisplaySettingsUpgradeAndReopen(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	s, err := OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	if err = s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	// Reproduce an existing installation with no media-display preference columns.
	for _, column := range []string{"video_default_muted", "video_loop", "image_cache_seconds", "video_cache_seconds"} {
		if _, err = s.DB.Exec("ALTER TABLE system_settings DROP COLUMN " + column); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadSettings(cfg)
	if err != nil || !loaded.VideoMutedByDefault() || loaded.VideoLoop || loaded.ImageCacheSeconds != 604800 || loaded.VideoCacheSeconds != 600 {
		t.Fatal("upgrade defaults not applied", err)
	}
	muted := false
	loaded.VideoDefaultMuted = &muted
	loaded.VideoLoop = true
	loaded.ImageCacheSeconds = 48 * 3600
	loaded.VideoCacheSeconds = 300
	if err = s.SaveSettings(loaded); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = s.LoadSettings(cfg)
	if err != nil || loaded.VideoMutedByDefault() || !loaded.VideoLoop || loaded.ImageCacheSeconds != 48*3600 || loaded.VideoCacheSeconds != 300 {
		t.Fatal("reopen reset saved preferences", err)
	}
}
