package archive

import "testing"

func TestSessionSettingsUpgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	if err = s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("ALTER TABLE system_settings DROP COLUMN session_ttl_seconds"); err != nil {
		t.Fatal(err)
	}
	if err = migrateSettings(s.DB); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadSettings(cfg)
	if err != nil || loaded.SessionTTLSeconds != 43200 {
		t.Fatal("upgrade did not preserve default login duration", err)
	}
	loaded.SessionTTLSeconds = 604800
	if err = s.SaveSettings(loaded); err != nil {
		t.Fatal(err)
	}
	if err = migrateSettings(s.DB); err != nil {
		t.Fatal(err)
	}
	loaded, err = s.LoadSettings(cfg)
	if err != nil || loaded.SessionTTLSeconds != 604800 {
		t.Fatal("repeat migration lost login duration", err)
	}
	loaded.SessionTTLSeconds = 3601
	if err = s.SaveSettings(loaded); err == nil {
		t.Fatal("unsupported login duration persisted")
	}
}
