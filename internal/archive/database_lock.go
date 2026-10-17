package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"sync"
	"time"
)

// A dedicated server connection holds a named lock across hosts. Losing it
// cancels the worker, rather than letting a reconnected process run unguarded.
func (s *Store) AcquireDatabaseWorker(ctx context.Context, cancel context.CancelFunc, databaseName string) (func(), error) {
	if s.DB.Dialect != "mysql" {
		return func() {}, nil
	}
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, errors.New("无法建立 MySQL 工作锁连接")
	}
	hash := sha256.Sum256([]byte(databaseName))
	name := "xm:" + hex.EncodeToString(hash[:24])
	var ok sql.NullInt64
	if err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?,0)", name).Scan(&ok); err != nil || !ok.Valid || ok.Int64 != 1 {
		conn.Close()
		return nil, errors.New("MySQL 数据库已有 XM 写入进程运行，或无法获取工作锁")
	}
	var owner int64
	if err = conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&owner); err != nil {
		conn.Close()
		return nil, errors.New("无法验证 MySQL 工作锁")
	}
	stop, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, finish := context.WithTimeout(ctx, 4*time.Second)
				var held sql.NullInt64
				err := conn.QueryRowContext(check, "SELECT IS_USED_LOCK(?)", name).Scan(&held)
				finish()
				if err != nil || !held.Valid || held.Int64 != owner {
					if ctx.Err() == nil {
						log.Print("MySQL 工作锁连接丢失，正在停止服务；请检查数据库连接后重启")
					}
					cancel()
					return
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			cleanup, finish := context.WithTimeout(context.Background(), 3*time.Second)
			defer finish()
			_, _ = conn.ExecContext(cleanup, "SELECT RELEASE_LOCK(?)", name)
			conn.Close()
		})
	}, nil
}
