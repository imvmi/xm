package archive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const mediaStagingMetadataReserve int64 = 64 << 10

// Bound abandoned download data without evicting the only complete local copy.
// A larger per-file setting also raises the allowance to fit one such file.
func MediaStagingLimit(cfg Config) int64 {
	return max(2<<30, cfg.MaxMediaBytes+mediaStagingMetadataReserve)
}

type MediaStagingUsage struct {
	Bytes         int64 `json:"bytes"`
	PartialBytes  int64 `json:"partial_bytes"`
	CompleteBytes int64 `json:"complete_bytes"`
	Files         int   `json:"files"`
}

type stagingFile struct {
	name     string
	info     fs.FileInfo
	complete bool
}

var stagingPartialName = regexp.MustCompile(`^\.download-[a-f0-9]{64}\.part(?:\.json)?$`)
var stagingMetadataName = regexp.MustCompile(`^\.resume-[0-9]+$`)

func (s *Store) stagingFiles(ctx context.Context, root *os.Root) ([]stagingFile, error) {
	files := []stagingFile{}
	seen := map[string]bool{}
	add := func(name string, complete bool) error {
		if seen[name] {
			return nil
		}
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		seen[name] = true
		files = append(files, stagingFile{name, info, complete})
		return nil
	}
	err := fs.WalkDir(root.FS(), "media/users", func(name string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		base := path.Base(name)
		if !stagingPartialName.MatchString(base) && !stagingMetadataName.MatchString(base) {
			return nil
		}
		complete := false
		if stagingPartialName.MatchString(base) {
			meta := strings.TrimSuffix(name, ".json") + ".json"
			f, err := root.Open(meta)
			if err == nil {
				var m mediaCheckpoint
				data, readErr := io.ReadAll(io.LimitReader(f, 16385))
				f.Close()
				complete = readErr == nil && len(data) <= 16384 && json.Unmarshal(data, &m) == nil && m.Bytes > 0 && m.Bytes == m.Total && len(m.SHA256) == 64
				// A sidecar left after file publication is disposable metadata. Only
				// protect a complete checkpoint while its actual .part still exists.
				if complete {
					partInfo, partErr := root.Lstat(strings.TrimSuffix(name, ".json"))
					complete = partErr == nil && partInfo.Mode().IsRegular()
				}
			}
		}
		return add(name, complete)
	})
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT local_path FROM media WHERE status<>'done' AND local_path<>''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		if safeObjectKey(name) && strings.HasPrefix(name, "media/") {
			if err = add(name, true); err != nil {
				return nil, err
			}
		}
	}
	return files, rows.Err()
}

func stagingUsage(files []stagingFile) MediaStagingUsage {
	var usage MediaStagingUsage
	for _, f := range files {
		usage.Bytes += f.info.Size()
		usage.Files++
		if f.complete {
			usage.CompleteBytes += f.info.Size()
		} else {
			usage.PartialBytes += f.info.Size()
		}
	}
	return usage
}

func (s *Store) MediaStagingUsage(ctx context.Context, dir string) (MediaStagingUsage, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return MediaStagingUsage{}, err
	}
	defer root.Close()
	files, err := s.stagingFiles(ctx, root)
	return stagingUsage(files), err
}

// Snapshot once per queue batch, then refresh only the current media's files.
// Scanning the entire archive for every downloaded image would be quadratic.
type mediaStagingBudget struct {
	root  *os.Root
	sizes map[string]int64
	limit int64
}

func (s *Store) newMediaStagingBudget(ctx context.Context, cfg Config) (*mediaStagingBudget, error) {
	root, err := os.OpenRoot(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	files, err := s.stagingFiles(ctx, root)
	if err != nil {
		root.Close()
		return nil, err
	}
	b := &mediaStagingBudget{root: root, sizes: map[string]int64{}, limit: MediaStagingLimit(cfg)}
	for _, f := range files {
		b.sizes[f.name] = f.info.Size()
	}
	return b, nil
}
func stagingPartPath(j MediaJob) string {
	rel, _ := mediaRelativePath(j, ".mp4")
	return path.Join(path.Dir(rel), ".download-"+strings.TrimSuffix(path.Base(rel), ".mp4")+".part")
}
func (b *mediaStagingBudget) capacity(j MediaJob) int64 {
	part := stagingPartPath(j)
	used := int64(0)
	for name, size := range b.sizes {
		if name != part && name != part+".json" {
			used += size
		}
	}
	return max(0, b.limit-used-mediaStagingMetadataReserve)
}
func (b *mediaStagingBudget) refresh(j MediaJob, localPath string, committed bool) {
	part := stagingPartPath(j)
	for _, name := range []string{part, part + ".json", j.Path, localPath} {
		if name == "" {
			continue
		}
		if committed && (name == j.Path || name == localPath) {
			delete(b.sizes, name)
			continue
		}
		info, err := b.root.Lstat(name)
		if err == nil && info.Mode().IsRegular() {
			b.sizes[name] = info.Size()
		} else if errors.Is(err, os.ErrNotExist) {
			delete(b.sizes, name)
		}
	}
}

var ErrMediaStagingBusy = errors.New("媒体归档正在进行，请结束后再清理断点")

// Clear only recognizable incomplete download files; completed staging and
// archived media are preserved. The worker lock excludes another XM process.
func (s *Store) ClearMediaPartials(ctx context.Context, dir string) (int64, error) {
	if !s.mediaStagingMu.TryLock() {
		return 0, ErrMediaStagingBusy
	}
	defer s.mediaStagingMu.Unlock()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	files, err := s.stagingFiles(ctx, root)
	if err != nil {
		return 0, err
	}
	var removed int64
	for _, file := range files {
		if file.complete {
			continue
		}
		if err = ctx.Err(); err != nil {
			return removed, err
		}
		current, e := root.Lstat(file.name)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return removed, e
		}
		if !current.Mode().IsRegular() || !os.SameFile(file.info, current) || file.info.Size() != current.Size() || !file.info.ModTime().Equal(current.ModTime()) {
			return removed, ErrMediaStagingBusy
		}
		// A rooted path prevents symlinks from escaping the data directory.
		if err = root.Remove(filepath.FromSlash(file.name)); err != nil {
			return removed, err
		}
		removed += file.info.Size()
	}
	return removed, nil
}
