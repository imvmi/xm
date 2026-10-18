package archive

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Only opaque pagination tokens are stored; request URLs and credentials are
// always supplied by the currently authenticated X page.
type TimelineProgress struct {
	Cursor string `json:"cursor,omitempty"`
	Done   bool   `json:"done,omitempty"`
	Head   string `json:"head,omitempty"`
}

type CollectionProgress struct {
	UserID  string           `json:"user_id"`
	Posts   TimelineProgress `json:"posts"`
	Replies TimelineProgress `json:"replies"`
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
