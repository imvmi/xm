package archive

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Only opaque pagination tokens are stored; request URLs and credentials are
// always supplied by the currently authenticated X page.
type TimelineProgress struct {
	Cursor    string `json:"cursor,omitempty"`
	Done      bool   `json:"done,omitempty"`
	Head      string `json:"head,omitempty"`
	ReachedAt string `json:"reached_at,omitempty"`
}

type CollectionProgress struct {
	UserID  string           `json:"user_id"`
	Posts   TimelineProgress `json:"posts"`
	Replies TimelineProgress `json:"replies"`
	SavedAt int64            `json:"saved_at,omitempty"`
}

// advanceReachedAt records the oldest item at the tail of a fully processed
// timeline page. The tail is used instead of the minimum timestamp because X
// may pin an older post near the top of a page.
func advanceReachedAt(current string, posts []Post) string {
	for i := len(posts) - 1; i >= 0; i-- {
		candidate, err := time.Parse(time.RubyDate, posts[i].CreatedAt)
		if err != nil {
			candidate, err = time.Parse(time.RFC3339, posts[i].CreatedAt)
		}
		if err != nil {
			continue
		}
		candidate = candidate.UTC()
		if previous, parseErr := time.Parse(time.RFC3339, current); parseErr == nil && !candidate.Before(previous) {
			return current
		}
		return candidate.Format(time.RFC3339)
	}
	return current
}

func (s *Store) LoadCollectionProgress(target, userID string) (CollectionProgress, error) {
	var raw string
	err := s.DB.QueryRow("SELECT state FROM collection_progress WHERE target=?", target).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return CollectionProgress{}, nil
	}
	if err != nil {
		return CollectionProgress{}, err
	}
	var p CollectionProgress
	if json.Unmarshal([]byte(raw), &p) != nil || !userIDPattern.MatchString(p.UserID) || p.UserID != userID {
		return p, fail("checkpoint", "分页断点与目标身份不符，已暂停；请重置断点后重试")
	}
	return p, nil
}
