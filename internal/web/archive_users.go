package web

import (
	"context"
	"net/http"
	"strings"

	"xm/internal/archive"
	"xm/internal/database"
)

type ArchiveUserStats struct {
	Username  string `json:"username"`
	Posts     int    `json:"posts"`
	Media     int    `json:"media"`
	LastSaved string `json:"last_saved"`
}

func archiveUsersCTE(target string) (string, []any) {
	if target != "" {
		return `WITH users(username) AS (SELECT target FROM archive_accounts WHERE target=? UNION SELECT username FROM monitors WHERE username=?) `, []any{target, target}
	}
	return `WITH users(username) AS (SELECT target FROM archive_accounts UNION SELECT username FROM monitors) `, nil
}

// The directory never aggregates historical posts or media. Identity and the
// latest profile are fetched in one round trip, including archived-only users.
func archiveUserDirectory(db *database.DB, target string) ([]UserView, error) {
	cte, args := archiveUsersCTE(target)
	return archiveUserDirectoryQuery(db, cte, args)
}

func archiveUserDirectoryQuery(db *database.DB, cte string, args []any) ([]UserView, error) {
	rows, err := db.Query(cte+`SELECT u.username,COALESCE(i.user_id,''),COALESCE(i.current_username,u.username),
 m.username IS NOT NULL,COALESCE(m.enabled,0),COALESCE(m.last_finished,0),COALESCE(m.show_in_timeline,a.show_in_timeline,1),
 COALESCE(NULLIF(m.interval_override_seconds,0),(SELECT default_interval_seconds FROM system_settings WHERE id=1),0),COALESCE(p.raw_json,''),COALESCE(f.protected,0)
 FROM users u LEFT JOIN target_identities i ON i.target=u.username
 LEFT JOIN monitors m ON m.username=u.username
 LEFT JOIN account_preferences a ON a.target=u.username
 LEFT JOIN posts p ON p.id=(SELECT t.post_id FROM target_posts t WHERE t.target=u.username ORDER BY `+timelineIDLength(db, "t.post_id")+` DESC,t.post_id DESC LIMIT 1)
 LEFT JOIN profiles f ON f.id=p.author_id ORDER BY u.username`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []UserView{}
	for rows.Next() {
		u := UserView{StatsPending: true}
		var raw string
		if err := rows.Scan(&u.Username, &u.UserID, &u.CurrentUsername, &u.Monitored, &u.Enabled, &u.LastRun, &u.ShowInTimeline, &u.Interval, &raw, &u.Protected); err != nil {
			return nil, err
		}
		u.Name, u.Bio = archive.AuthorDetails([]byte(raw))
		if u.Name == "" {
			u.Name = u.CurrentUsername
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// Task records keep their archive keys, even after monitoring is removed. Resolve
// their current labels in batches using the same profile source as the directory.
func taskProfiles(db *database.DB, targets []string) (map[string]UserView, error) {
	profiles := make(map[string]UserView)
	seen := make(map[string]bool)
	unique := make([]string, 0, len(targets))
	for _, target := range targets {
		if !seen[target] {
			seen[target] = true
			unique = append(unique, target)
		}
	}
	for start := 0; start < len(unique); start += 500 {
		chunk := unique[start:min(start+500, len(unique))]
		args := make([]any, len(chunk))
		selects := make([]string, len(chunk))
		for i, target := range chunk {
			args[i] = target
			selects[i] = "SELECT ?"
		}
		users, err := archiveUserDirectoryQuery(db, "WITH users(username) AS ("+strings.Join(selects, " UNION ALL ")+") ", args)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			profiles[u.Username] = u
		}
	}
	return profiles, nil
}

// A correlated count drives the lookup from the account's distinct related
// posts into media_post_status. A plain join lets SQLite scan all done media
// for every account. UNION (not UNION ALL) also terminates quote cycles and
// counts media shared by several roots only once within each account.
const archiveMediaCountSQL = `WITH RECURSIVE related(id) AS (
 SELECT post_id FROM target_posts WHERE target=?
 UNION SELECT p.quote_id FROM related r JOIN posts p ON p.id=r.id WHERE p.quote_id<>''
) SELECT COALESCE(SUM((SELECT COUNT(*) FROM media a WHERE a.post_id=r.id AND a.status='done')),0) FROM related r`

func archiveUserStats(db *database.DB, target string) ([]ArchiveUserStats, error) {
	return archiveUserStatsContext(context.Background(), db, target)
}

func archiveUserStatsContext(ctx context.Context, db *database.DB, target string) ([]ArchiveUserStats, error) {
	cte, args := archiveUsersCTE(target)
	rows, err := db.QueryContext(ctx, cte+`SELECT u.username,COUNT(t.post_id),COALESCE(MAX(p.first_seen),'')
 FROM users u LEFT JOIN target_posts t ON t.target=u.username LEFT JOIN posts p ON p.id=t.post_id
 GROUP BY u.username ORDER BY u.username`, args...)
	if err != nil {
		return nil, err
	}
	stats := []ArchiveUserStats{}
	for rows.Next() {
		var s ArchiveUserStats
		if err = rows.Scan(&s.Username, &s.Posts, &s.LastSaved); err != nil {
			rows.Close()
			return nil, err
		}
		stats = append(stats, s)
	}
	err = rows.Err()
	rows.Close() // SQLite has a single connection; release before nested queries.
	if err != nil {
		return nil, err
	}
	for i := range stats {
		if err = db.QueryRowContext(ctx, archiveMediaCountSQL, stats[i].Username).Scan(&stats[i].Media); err != nil {
			return nil, err
		}
	}
	return stats, nil
}

func (a *App) userStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	target := r.URL.Query().Get("target")
	force := r.URL.Query().Get("refresh") == "1"
	if target != "" && force {
		defer a.userStatsCache.invalidate()
	}
	load := func() ([]ArchiveUserStats, error) { return a.loadAccountStats(ctx, target, force) }
	var stats []ArchiveUserStats
	var err error
	if target != "" {
		stats, err = load()
	} else {
		stats, err = a.userStatsCache.get(ctx, force, load)
	}
	if err != nil {
		problem(w, 500, "读取归档统计失败")
		return
	}
	respond(w, 200, stats)
}

func (a *App) loadAccountStats(ctx context.Context, target string, force bool) ([]ArchiveUserStats, error) {
	cte, args := archiveUsersCTE(target)
	rows, err := a.store.DB.QueryContext(ctx, cte+"SELECT username FROM users ORDER BY username", args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	stats := []ArchiveUserStats{}
	for _, name := range names {
		items, err := a.accountStatsCache(name).get(ctx, force, func() ([]ArchiveUserStats, error) { return archiveUserStatsContext(ctx, a.store.DB, name) })
		if err != nil {
			return nil, err
		}
		stats = append(stats, items...)
	}
	return stats, nil
}
