package archive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMediaStagingCleanupPreservesCompleteAndArchivedFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j := resumeMediaFixture()
	interruptedPrefix(t, dir, j)
	usage, err := s.MediaStagingUsage(context.Background(), dir)
	if err != nil || usage.PartialBytes < 3 || usage.CompleteBytes != 0 {
		t.Fatal(usage, err)
	}
	folder := filepath.Join(dir, "media", "users", "alice_42")
	complete := filepath.Join(folder, ".download-"+strings.Repeat("c", 64)+".part")
	if err = os.WriteFile(complete, []byte("complete"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(mediaCheckpoint{Bytes: 8, Total: 8, SHA256: strings.Repeat("a", 64)})
	if err = os.WriteFile(complete+".json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"200", "300"} {
		if err = s.SavePosts("alice", []Post{{ID: id, Username: "alice", Assets: []Asset{{ID: "v", Kind: "video"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"staged.mp4", "archived.mp4", "unrelated.txt"} {
		if err = os.WriteFile(filepath.Join(folder, name), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.DB.Exec("UPDATE media SET status='ignored',local_path='media/users/alice_42/staged.mp4' WHERE post_id='200'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE media SET status='done',local_path='media/users/alice_42/archived.mp4' WHERE post_id='300'"); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(folder, ".download-"+strings.Repeat("d", 64)+".part.json")
	if err = os.WriteFile(orphan, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := s.MediaStagingUsage(context.Background(), dir)
	if err != nil || before.CompleteBytes < 12 {
		t.Fatal(before, err)
	}
	removed, err := s.ClearMediaPartials(context.Background(), dir)
	if err != nil || removed != before.PartialBytes {
		t.Fatal(removed, before, err)
	}
	after, err := s.MediaStagingUsage(context.Background(), dir)
	if err != nil || after.PartialBytes != 0 || after.CompleteBytes != before.CompleteBytes {
		t.Fatal(after, before, err)
	}
	for _, name := range []string{complete, complete + ".json", filepath.Join(folder, "staged.mp4"), filepath.Join(folder, "archived.mp4"), filepath.Join(folder, "unrelated.txt")} {
		if _, err = os.Stat(name); err != nil {
			t.Fatal(name, err)
		}
	}
	s.mediaStagingMu.Lock()
	_, err = s.ClearMediaPartials(context.Background(), dir)
	s.mediaStagingMu.Unlock()
	if !errors.Is(err, ErrMediaStagingBusy) {
		t.Fatal(err)
	}
}

func TestMediaStagingBudgetRejectsNewDownloadWithoutEviction(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		dir := t.TempDir()
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		j := resumeMediaFixture()
		folder := filepath.Join(dir, "media", "users", "alice_42")
		if err = PrivateDir(folder); err != nil {
			t.Fatal(err)
		}
		old := filepath.Join(folder, ".download-"+strings.Repeat("b", 64)+".part")
		f, err := os.Create(old)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.Truncate((2 << 30) - mediaStagingMetadataReserve - 4); err != nil {
			t.Fatal(err)
		}
		f.Close()
		cfg := Config{DataDir: dir, MaxMediaBytes: 1024}
		budget, err := s.newMediaStagingBudget(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		capacity := budget.capacity(j)
		if capacity != 4 {
			t.Fatal(capacity)
		}
		c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			n := int64(6)
			if unknownLength {
				n = -1
			}
			return mediaResponse(200, "", `"v1"`, strings.NewReader("abcdef"), n), nil
		})}
		_, _, _, err = downloadMediaWithCapacity(context.Background(), c, dir, j, 1024, capacity, nil)
		if !errors.Is(err, errMediaStagingFull) || IsPermanentMediaError(err) {
			t.Fatal(err)
		}
		if info, err := os.Stat(old); err != nil || info.Size() != (2<<30)-mediaStagingMetadataReserve-4 {
			t.Fatal(info, err)
		}
		budget.root.Close()
		s.Close()
	}
}

func TestMediaStagingBudgetReleasesCommittedFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	budget, err := s.newMediaStagingBudget(context.Background(), Config{DataDir: dir, MaxMediaBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer budget.root.Close()
	budget.limit = mediaStagingMetadataReserve + 10
	j := resumeMediaFixture()
	part := stagingPartPath(j)
	if err = PrivateDir(filepath.Join(dir, filepath.Dir(part))); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, part), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	budget.refresh(j, "", false)
	next := j
	next.ID = "next"
	if budget.capacity(next) != 7 || budget.capacity(j) != 10 {
		t.Fatal(budget.sizes)
	}
	rel, _ := mediaRelativePath(j, ".mp4")
	if err = os.Rename(filepath.Join(dir, part), filepath.Join(dir, rel)); err != nil {
		t.Fatal(err)
	}
	budget.refresh(j, rel, true)
	if budget.capacity(next) != 10 {
		t.Fatal("archived content charged against staging quota", budget.sizes)
	}
}
