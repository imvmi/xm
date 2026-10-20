package archive

import (
	"strings"
	"testing"
)

func TestLegacyStorageSchemaUpgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.StorageProfiles = []StorageProfile{{ID: "dav", Name: "existing", Kind: "webdav", Endpoint: "https://example.com/dav", Timeout: 5, Username: "user", Password: "preserved-secret"}}
	cfg.MediaStorage = "dav"
	if err = s.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = s.DB.QueryRow("SELECT sql FROM sqlite_master WHERE name='storage_profiles'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(schema, "'s3','smb'", "'s3'", 1)
	legacy = strings.Replace(legacy, "storage_profiles", "storage_profiles_old", 1)
	for _, q := range []string{"PRAGMA foreign_keys=OFF", legacy, "INSERT INTO storage_profiles_old SELECT * FROM storage_profiles", "DROP TABLE storage_profiles", "ALTER TABLE storage_profiles_old RENAME TO storage_profiles", "PRAGMA foreign_keys=ON"} {
		if _, err = s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal("opening existing database", err)
	}
	defer s.Close()
	loaded, err := s.LoadSettings(cfg)
	if err != nil || loaded.ActiveStorage() != "dav" || loaded.StorageProfiles[0].Password != "preserved-secret" {
		t.Fatal("lost existing storage configuration", err)
	}
	loaded.StorageProfiles = append(loaded.StorageProfiles, smbProfile("smb://nas/share"))
	loaded.MediaStorage = "samba"
	if err = s.SaveSettings(loaded); err != nil {
		t.Fatal("upgraded constraint rejected SMB", err)
	}
	var enabled int
	s.DB.QueryRow("PRAGMA foreign_keys").Scan(&enabled)
	if enabled != 1 {
		t.Fatal("foreign key enforcement not restored")
	}
	if _, err = s.DB.Exec("UPDATE system_settings SET active_storage_id='missing'"); err == nil {
		t.Fatal("foreign key constraint was lost")
	}
	if err = upgradeStorageKinds(s.DB); err != nil {
		t.Fatal("upgrade is not idempotent", err)
	}
}
