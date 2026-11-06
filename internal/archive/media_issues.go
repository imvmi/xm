package archive

import (
	"context"
	"database/sql"
	"errors"
)

type MediaProblem struct {
	PostID      string `json:"post_id"`
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	Attempts    int    `json:"attempts"`
	NextAttempt int64  `json:"next_attempt"`
	UpdatedAt   string `json:"updated_at"`
}

func (s *Store) IgnoredMedia(target string) (int, error) {
	var count int
	err := s.DB.QueryRow("SELECT COUNT(*) FROM media WHERE status='ignored'"+targetMediaFilter, target).Scan(&count)
	return count, err
}

// MediaProblems returns current unresolved and manually ignored media, including
// quoted posts. It deliberately omits source URLs and storage credentials.
func (s *Store) MediaProblems(ctx context.Context, target string, limit, offset int) ([]MediaProblem, int, error) {
	where := " FROM media WHERE status<>'done'" + targetMediaFilter
	var total int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*)"+where, target).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT post_id,media_id,kind,status,last_error,attempts,next_attempt,updated_at"+where+" ORDER BY post_id,media_id LIMIT ? OFFSET ?", target, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []MediaProblem{}
	for rows.Next() {
		var item MediaProblem
		if err := rows.Scan(&item.PostID, &item.ID, &item.Kind, &item.Status, &item.Error, &item.Attempts, &item.NextAttempt, &item.UpdatedAt); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

var ErrMediaAlreadySaved = errors.New("媒体已经保存成功，无需忽略或重试")

// SetMediaIgnored must be called while collection is idle. The account scope
// protects against editing an unrelated file through another run's dialog.
func (s *Store) SetMediaIgnored(target, postID, mediaID string, ignore bool) error {
	where := " WHERE post_id=? AND media_id=?" + targetMediaFilter
	var current string
	if err := s.DB.QueryRow("SELECT status FROM media"+where, postID, mediaID, target).Scan(&current); err != nil {
		return err
	}
	if current == "done" {
		return ErrMediaAlreadySaved
	}
	status := "pending"
	if ignore {
		status = "ignored"
	}
	// Preserve the original error, attempts, URL, and any local staging file.
	result, err := s.DB.Exec("UPDATE media SET status=?,next_attempt=0 WHERE status<>'done' AND post_id=? AND media_id=?"+targetMediaFilter, status, postID, mediaID, target)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count == 0 && current != status {
		return sql.ErrNoRows
	}
	return nil
}
