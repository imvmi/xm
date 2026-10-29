package archive

import "testing"

func TestQuoteDisplaySettingsUpgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := DefaultConfig()
	if err = s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	// Simulate an existing installation without the new preference column.
	if _, err = s.DB.Exec("ALTER TABLE system_settings DROP COLUMN quote_display_depth"); err != nil {
		t.Fatal(err)
	}
	if err = migrateSettings(s.DB); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadSettings(cfg)
	if err != nil || loaded.QuoteDepthLimit() != 4 {
		t.Fatal("upgrade changed the default", err)
	}
	depth := 0
	loaded.QuoteDisplayDepth = &depth
	if err = s.SaveSettings(loaded); err != nil {
		t.Fatal(err)
	}
	if err = migrateSettings(s.DB); err != nil {
		t.Fatal(err)
	}
	loaded, err = s.LoadSettings(cfg)
	if err != nil || loaded.QuoteDepthLimit() != 0 {
		t.Fatal("repeat migration lost explicit zero", err)
	}
	for _, invalid := range []int{-1, 5} {
		loaded.QuoteDisplayDepth = &invalid
		if err = s.SaveSettings(loaded); err == nil {
			t.Fatal("invalid depth saved", invalid)
		}
	}
}
