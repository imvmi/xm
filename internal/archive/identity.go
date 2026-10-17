package archive

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"xm/internal/database"
)

var userIDPattern = regexp.MustCompile(`^[0-9]{1,64}$`)

// TargetIdentity keeps the archive key stable when the public handle changes.
type TargetIdentity struct {
	UserID          string `json:"user_id"`
	CurrentUsername string `json:"current_username"`
	ReferencePostID string `json:"-"`
}

func (s *Store) TargetIdentity(target string) (TargetIdentity, error) {
	var identity TargetIdentity
	err := s.DB.QueryRow("SELECT user_id,current_username FROM target_identities WHERE target=?", target).Scan(&identity.UserID, &identity.CurrentUsername)
	if err == nil {
		return s.identityAnchor(target, identity)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return identity, err
	}
	// Recover an unambiguous identity from previous captures, never from the
	// current owner of an old handle. Quotes are excluded by target_posts.
	rows, err := s.DB.Query(`SELECT id FROM profiles WHERE lower(username)=? UNION SELECT p.author_id FROM posts p JOIN target_posts t ON t.post_id=p.id WHERE t.target=? AND lower(p.username)=?`, target, target, target)
	if err != nil {
		return identity, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return identity, err
		}
		if id != "" {
			ids[id] = true
		}
	}
	if err = rows.Err(); err != nil {
		return identity, err
	}
	if len(ids) > 1 {
		return identity, fail("identity", "历史归档存在多个用户 ID，已停止采集，请检查账号身份")
	}
	for id := range ids {
		identity.UserID = id
	}
	identity.CurrentUsername = target
	rows.Close()
	return s.identityAnchor(target, identity)
}

func (s *Store) SaveTargetProfile(target string, p Profile) error {
	if !userIDPattern.MatchString(p.ID) || !usernamePattern.MatchString(p.Username) {
		return fail("identity", "用户资料缺少有效的 ID 或用户名，已停止采集")
	}
	old, err := s.TargetIdentity(target)
	if err != nil {
		return err
	}
	if old.UserID != "" && old.UserID != p.ID {
		return fail("identity", "目标用户 ID 与绑定身份不符，已停止采集以避免混入其他账号")
	}
	if old.UserID == "" && !strings.EqualFold(target, p.Username) {
		return fail("identity", "首次采集的用户名不符，无法绑定身份")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.InsertIgnore("INSERT INTO target_identities(target,user_id,current_username) VALUES(?,?,?)", "target", target, p.ID, strings.ToLower(p.Username)); err != nil {
		return err
	}
	var bound string
	if err = tx.QueryRow("SELECT user_id FROM target_identities WHERE target=?", target).Scan(&bound); err != nil {
		return err
	}
	if bound != p.ID {
		return fail("identity", "用户 ID 与绑定身份不符，已停止采集")
	}
	if _, err = tx.Exec("UPDATE target_identities SET current_username=? WHERE target=?", strings.ToLower(p.Username), target); err != nil {
		return err
	}
	if _, err = tx.Exec(database.Upsert(tx.Dialect, `INSERT INTO profiles VALUES(?,?,?,?)`, []string{"id"}, []string{"username", "protected", "checked_at"}), p.ID, p.Username, p.Protected, now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) identityAnchor(target string, identity TargetIdentity) (TargetIdentity, error) {
	if identity.UserID == "" {
		return identity, nil
	}
	err := s.DB.QueryRow(`SELECT p.id FROM posts p JOIN target_posts t ON t.post_id=p.id WHERE t.target=? AND p.author_id=? ORDER BY length(p.id) DESC,p.id DESC LIMIT 1`, target, identity.UserID).Scan(&identity.ReferencePostID)
	if errors.Is(err, sql.ErrNoRows) {
		return identity, nil
	}
	return identity, err
}

// IdentityFromPost resolves only the known archived post, not recommendations,
// replies or another user's quote. Its author ID must still match the binding.
func IdentityFromPost(data []byte, postID string) (Profile, error) {
	var root any
	if json.Unmarshal(data, &root) != nil {
		return Profile{}, fail("identity", "无法解析归档推文的作者身份")
	}
	if len(array(at(root, "errors"))) > 0 {
		_, err := ParsePage(data, "UserByScreenName")
		return Profile{}, err
	}
	var found *Profile
	var walk func(any)
	walk = func(v any) {
		if found != nil {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			if str(x["rest_id"]) == postID {
				u := at(x, "core", "user_results", "result")
				id := str(at(u, "rest_id"))
				name := first(str(at(u, "core", "screen_name")), str(at(u, "legacy", "screen_name")))
				if userIDPattern.MatchString(id) && usernamePattern.MatchString(name) {
					found = &Profile{ID: id, Username: strings.ToLower(name)}
					return
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(root)
	if found == nil {
		return Profile{}, fail("identity", "用于核对身份的归档推文已删除或不可访问，未继续按旧用户名采集")
	}
	return *found, nil
}
