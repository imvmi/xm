package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CopyToSQLite requires an empty, fully initialized destination and quiescent
// source writers. It never modifies the source. Progress excludes credentials.
// Unlike SQL dumps, explicit columns and destination affinities preserve blobs,
// integer booleans, Unicode, NULL and long string IDs across the two drivers.
func (s *Store) CopyToSQLite(ctx context.Context, dst *Store, progress func(string, int64, int64) error) error {
	if dst.DB.Dialect != "sqlite" {
		return errors.New("迁移目标必须是 SQLite")
	}
	return s.CopyDatabase(ctx, dst, progress)
}

// CopyDatabase copies and verifies either supported dialect in one destination
// transaction. Both stores must be initialized and their writers quiescent.
func (s *Store) CopyDatabase(ctx context.Context, dst *Store, progress func(string, int64, int64) error) error {
	if dst.DB.Dialect != "sqlite" && dst.DB.Dialect != "mysql" {
		return errors.New("不支持的迁移目标数据库")
	}
	src, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return errors.New("无法建立源数据库快照")
	}
	defer src.Rollback()
	tx, err := dst.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	counts := make(map[string]int64)
	var total, done int64
	for _, table := range archiveTables {
		var n int64
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&n); err != nil {
			return fmt.Errorf("目标表 %s 不可读", table)
		}
		if n != 0 {
			return fmt.Errorf("目标表 %s 非空，拒绝覆盖", table)
		}
		if err = src.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&n); err != nil {
			return fmt.Errorf("源表 %s 不可读", table)
		}
		counts[table], total = n, total+n
	}
	report := func(phase string) error {
		if progress != nil {
			return progress(phase, done, total*2)
		}
		return ctx.Err()
	}
	for _, table := range archiveTables {
		if err = report("复制 " + table); err != nil {
			return err
		}
		meta, e := tx.QueryContext(ctx, "SELECT * FROM `"+table+"` WHERE 1=0")
		if e != nil {
			return e
		}
		cols, e := meta.Columns()
		if e != nil {
			meta.Close()
			return e
		}
		types, e := meta.ColumnTypes()
		meta.Close()
		if e != nil {
			return e
		}
		sourceMeta, e := src.QueryContext(ctx, "SELECT * FROM `"+table+"` WHERE 1=0")
		if e != nil {
			return fmt.Errorf("无法检查源表 %s 结构", table)
		}
		sourceColumns, e := sourceMeta.Columns()
		sourceMeta.Close()
		if e != nil || len(sourceColumns) != len(cols) {
			return fmt.Errorf("源表 %s 结构不兼容，拒绝遗漏字段", table)
		}
		known := make(map[string]bool, len(cols))
		for _, col := range cols {
			known[col] = true
		}
		for _, col := range sourceColumns {
			if !known[col] {
				return fmt.Errorf("源表 %s 结构不兼容，拒绝遗漏字段", table)
			}
		}
		selectSQL := "SELECT `" + strings.Join(cols, "`,`") + "` FROM `" + table + "`"
		rows, e := src.QueryContext(ctx, selectSQL)
		if e != nil {
			return fmt.Errorf("源表 %s 结构不兼容", table)
		}
		stmt, e := tx.PrepareContext(ctx, "INSERT INTO `"+table+"` (`"+strings.Join(cols, "`,`")+"`) VALUES ("+strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")+")")
		if e != nil {
			rows.Close()
			return e
		}
		var expected [][32]byte
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if e = rows.Scan(ptrs...); e != nil {
				break
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok && !strings.Contains(strings.ToUpper(types[i].DatabaseTypeName()), "BLOB") && !strings.Contains(strings.ToUpper(types[i].DatabaseTypeName()), "BINARY") {
					values[i] = string(b)
				}
			}
			if _, e = stmt.ExecContext(ctx, values...); e != nil {
				break
			}
			expected = append(expected, migrationRowHash(values))
			done++
			if done%250 == 0 {
				if e = report("复制 " + table); e != nil {
					break
				}
			}
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		stmt.Close()
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("复制表 %s 失败，源数据未修改", table)
		}
		if int64(len(expected)) != counts[table] {
			return fmt.Errorf("表 %s 快照行数不一致", table)
		}
		if err = report("校验 " + table); err != nil {
			return err
		}
		check, e := tx.QueryContext(ctx, selectSQL)
		if e != nil {
			return e
		}
		var actual [][32]byte
		for check.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if e = check.Scan(ptrs...); e != nil {
				break
			}
			actual = append(actual, migrationRowHash(values))
			done++
			if done%250 == 0 {
				if e = report("校验 " + table); e != nil {
					break
				}
			}
		}
		if e == nil {
			e = check.Err()
		}
		check.Close()
		if e != nil {
			return e
		}
		sort.Slice(expected, func(i, j int) bool { return bytes.Compare(expected[i][:], expected[j][:]) < 0 })
		sort.Slice(actual, func(i, j int) bool { return bytes.Compare(actual[i][:], actual[j][:]) < 0 })
		if len(actual) != len(expected) {
			return fmt.Errorf("表 %s 行数校验失败", table)
		}
		for i := range expected {
			if expected[i] != actual[i] {
				return fmt.Errorf("表 %s 内容校验失败", table)
			}
		}
	}
	if dst.DB.Dialect == "sqlite" {
		fk, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
		if err != nil {
			return err
		}
		bad := fk.Next()
		err = fk.Err()
		fk.Close()
		if err != nil {
			return err
		}
		if bad {
			return errors.New("SQLite 外键校验失败")
		}
		var integrity string
		if err = tx.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			return errors.New("SQLite 完整性校验失败")
		}
	}
	if err = report("校验完成"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return errors.New("数据库提交未确认，请检查目标数据库后再重试；源数据库保留")
	}
	return nil
}

func migrationRowHash(values []any) [32]byte {
	h := sha256.New()
	for _, v := range values {
		if v == nil {
			h.Write([]byte{0})
			continue
		}
		h.Write([]byte{1})
		var b []byte
		switch x := v.(type) {
		case []byte:
			b = x
		case bool:
			if x {
				b = []byte("1")
			} else {
				b = []byte("0")
			}
		default:
			b = []byte(fmt.Sprint(v))
		}
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(b)))
		h.Write(size[:])
		h.Write(b)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}
