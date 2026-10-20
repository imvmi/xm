package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"xm/internal/database"
)

func upgradeStorageKinds(db *database.DB) error {
	if db.Dialect == "mysql" {
		rows, err := db.Query(`SELECT tc.CONSTRAINT_NAME,cc.CHECK_CLAUSE
FROM information_schema.TABLE_CONSTRAINTS tc
JOIN information_schema.CHECK_CONSTRAINTS cc ON cc.CONSTRAINT_SCHEMA=tc.CONSTRAINT_SCHEMA AND cc.CONSTRAINT_NAME=tc.CONSTRAINT_NAME
WHERE tc.TABLE_SCHEMA=DATABASE() AND tc.TABLE_NAME='storage_profiles' AND tc.CONSTRAINT_TYPE='CHECK'`)
		if err != nil {
			return err
		}
		var old []string
		for rows.Next() {
			var name, clause string
			if err = rows.Scan(&name, &clause); err != nil {
				rows.Close()
				return err
			}
			if strings.Contains(strings.ToLower(clause), "kind") && !strings.Contains(clause, "'smb'") {
				old = append(old, name)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(old) == 0 {
			return err
		}
		var changes []string
		for _, name := range old {
			changes = append(changes, "DROP CHECK `"+strings.ReplaceAll(name, "`", "``")+"`")
		}
		changes = append(changes, "ADD CONSTRAINT storage_profiles_kind CHECK(kind IN ('local','webdav','s3','smb'))")
		_, err = db.Exec("ALTER TABLE storage_profiles " + strings.Join(changes, ","))
		return err
	}
	var schema string
	err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='storage_profiles'").Scan(&schema)
	if errors.Is(err, sql.ErrNoRows) || err == nil && strings.Contains(schema, "'smb'") {
		return nil
	}
	if err != nil {
		return err
	}
	// SQLite requires a rebuild to extend a CHECK constraint. Disable foreign
	// keys on this connection outside the transaction, then validate before commit.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "PRAGMA foreign_keys=ON")
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ddl string
	for _, q := range settingsSchema {
		if strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS storage_profiles(") {
			ddl = strings.Replace(q, "IF NOT EXISTS storage_profiles", "storage_profiles_smb_upgrade", 1)
		}
	}
	if ddl == "" {
		return errors.New("缺少存储表结构")
	}
	const cols = "id,name,kind,endpoint,object_prefix,username,password,region,bucket,access_key,secret_key,session_token,path_style,use_proxy,timeout_seconds,position,updated_at"
	for _, q := range []string{
		ddl,
		"INSERT INTO storage_profiles_smb_upgrade(" + cols + ") SELECT " + cols + " FROM storage_profiles",
		"DROP TABLE storage_profiles",
		"ALTER TABLE storage_profiles_smb_upgrade RENAME TO storage_profiles",
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	invalid := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if invalid {
		return errors.New("升级存储表时发现无效外键，已回滚")
	}
	return tx.Commit()
}
