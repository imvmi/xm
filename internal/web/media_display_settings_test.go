package web

import (
	"context"
	"encoding/json"
	"testing"

	"xm/internal/archive"
)

func TestMediaDisplaySettingsPersistAndControlCache(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	if !a.Config().VideoMutedByDefault() || a.Config().VideoLoop || a.Config().MediaPreloadMode != "standard" || a.Config().ImageCacheSeconds != imageCacheSeconds || a.Config().VideoCacheSeconds != videoCacheSeconds {
		t.Fatal("incorrect defaults", a.Config().VideoMutedByDefault(), a.Config().ImageCacheSeconds, a.Config().VideoCacheSeconds)
	}
	input := map[string]any{"video_default_muted": false, "video_loop": true, "media_preload_mode": "enhanced", "image_cache_seconds": 48 * 3600, "video_cache_seconds": 5 * 60}
	ok(t, b.request("PUT", "/api/settings", input, "http://localhost", true), 200)
	w := b.request("GET", "/api/settings", nil, "", false)
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["media_preload_mode"] != "enhanced" || got["video_default_muted"] != false || got["video_loop"] != true || got["image_cache_seconds"] != float64(48*3600) || got["video_cache_seconds"] != float64(300) {
		t.Fatal("settings API did not return saved values", got)
	}
	reloaded, err := a.store.LoadSettings(a.Config())
	if err != nil || reloaded.MediaPreloadMode != "enhanced" || reloaded.VideoMutedByDefault() || !reloaded.VideoLoop || reloaded.ImageCacheSeconds != 48*3600 || reloaded.VideoCacheSeconds != 300 {
		t.Fatal("preferences not persisted", err)
	}
	// Unrelated partial updates preserve playback and cache preferences.
	ok(t, b.request("PUT", "/api/settings", map[string]any{"archive_theme": "dark"}, "http://localhost", true), 200)
	for _, f := range []struct{ id, kind, ext, expected string }{{"100", "photo", ".jpg", "private, max-age=172800, must-revalidate"}, {"200", "video", ".mp4", "private, max-age=300, must-revalidate"}} {
		hash := seedCachedMedia(t, a, f.id, f.kind, f.ext, []byte("payload"))
		w := cachedMediaRequest(a, b, "GET", "/media/"+f.id+"/asset?v="+hash, nil)
		ok(t, w, 200)
		if w.Header().Get("Cache-Control") != f.expected {
			t.Fatal("saved cache time was ignored", w.Header())
		}
	}
	for _, invalid := range []map[string]any{
		{"image_cache_seconds": 0}, {"image_cache_seconds": -1}, {"image_cache_seconds": 3601}, {"image_cache_seconds": 31 * 24 * 3600},
		{"video_cache_seconds": 0}, {"video_cache_seconds": -1}, {"video_cache_seconds": 61}, {"video_cache_seconds": 86460},
		{"video_default_muted": "false"},
		{"video_loop": "true"}, {"media_preload_mode": ""}, {"media_preload_mode": "invalid"}, {"media_preload_mode": true},
	} {
		ok(t, b.request("PUT", "/api/settings", invalid, "http://localhost", true), 400)
	}
	if a.Config().VideoMutedByDefault() || !a.Config().VideoLoop || a.Config().VideoCacheSeconds != 300 {
		t.Fatal("invalid update changed saved preferences")
	}
	ok(t, b.request("PUT", "/api/settings", map[string]any{"video_default_muted": true, "video_loop": false}, "http://localhost", true), 200)
	if !a.Config().VideoMutedByDefault() || a.Config().VideoLoop {
		t.Fatal("mute setting cannot be turned back on")
	}
}

func TestMediaDisplaySettingsDatabaseCopy(t *testing.T) {
	a := testApp(t)
	b := login(t, a)
	ok(t, b.request("PUT", "/api/settings", map[string]any{"video_default_muted": false, "video_loop": true, "media_preload_mode": "enhanced", "image_cache_seconds": 7200, "video_cache_seconds": 120}, "http://localhost", true), 200)
	dst, err := archive.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = migrate(dst); err != nil {
		t.Fatal(err)
	}
	if err = a.store.CopyToSQLite(context.Background(), dst, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := dst.LoadSettings(archive.DefaultConfig())
	if err != nil || loaded.MediaPreloadMode != "enhanced" || loaded.VideoMutedByDefault() || !loaded.VideoLoop || loaded.ImageCacheSeconds != 7200 || loaded.VideoCacheSeconds != 120 {
		t.Fatal("database migration lost media preferences", err)
	}
}
