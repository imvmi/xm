package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

var mediaDirectoryPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func mediaOwner(j MediaJob) (string, string) {
	handle := strings.ToLower(j.Username)
	knownHandle := usernamePattern.MatchString(handle)
	if !knownHandle {
		handle = "user"
	}
	key := "id:" + j.AuthorID
	if j.AuthorID == "" {
		key = "username:" + handle
		if !knownHandle {
			key = "post:" + j.PostID
		}
	}
	sum := sha256.Sum256([]byte(key))
	token := j.AuthorID
	if !userIDPattern.MatchString(token) {
		token = hex.EncodeToString(sum[:8])
	}
	if len(key) > 128 {
		key = "hash:" + hex.EncodeToString(sum[:])
	}
	return key, handle + "_" + token
}

// The first directory name is immutable, even after a username change.
func (s *Store) MediaDirectory(j MediaJob) (string, error) {
	key, candidate := mediaOwner(j)
	if _, err := s.DB.InsertIgnore("INSERT INTO media_directories(owner_key,directory_name,created_at) VALUES(?,?,?)", "owner_key", key, candidate, now()); err != nil {
		return "", errors.New("保存媒体作者目录失败")
	}
	var name string
	if err := s.DB.QueryRow("SELECT directory_name FROM media_directories WHERE owner_key=?", key).Scan(&name); err != nil || !mediaDirectoryPattern.MatchString(name) {
		return "", errors.New("媒体作者目录无效")
	}
	return name, nil
}
func mediaRelativePath(j MediaJob, ext string) (string, error) {
	owner := j.Directory
	if owner == "" {
		_, owner = mediaOwner(j)
	}
	if !mediaDirectoryPattern.MatchString(owner) {
		return "", errors.New("媒体作者目录无效")
	}
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".mp4":
	default:
		return "", errors.New("媒体扩展名无效")
	}
	key := sha256.Sum256([]byte(j.PostID + "/" + j.ID))
	return filepath.ToSlash(filepath.Join("media", "users", owner, hex.EncodeToString(key[:])+ext)), nil
}
