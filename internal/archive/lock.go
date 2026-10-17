package archive

import (
	"errors"
	"path/filepath"

	"github.com/gofrs/flock"
)

// AcquireWorkerLock keeps a web scheduler and CLI collection from using the
// same account/data directory concurrently. OS locks are released on crashes.
func AcquireWorkerLock(dir string) (func(), error) {
	if err := PrivateDir(dir); err != nil {
		return nil, err
	}
	f := flock.New(filepath.Join(dir, "worker.lock"))
	ok, err := f.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		_ = f.Close()
		return nil, errors.New("此数据目录已有服务或采集任务运行；请通过管理页面操作，或先停止现有进程")
	}
	return func() { _ = f.Close() }, nil
}
