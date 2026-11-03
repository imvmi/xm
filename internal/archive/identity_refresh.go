package archive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IdentityReferencePosts returns a small set of this account's own archived
// posts. Quotes and unrelated authors must never supply a replacement identity.
func (s *Store) IdentityReferencePosts(target, userID string) ([]string, error) {
	rows, err := s.DB.Query(`SELECT p.id FROM posts p JOIN target_posts t ON t.post_id=p.id WHERE t.target=? AND p.author_id=? ORDER BY length(p.id) DESC,p.id DESC LIMIT 3`, target, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RefreshTargetProfile only resolves and verifies a profile; it never writes
// posts, changes checkpoints, or replaces the bound numeric user ID.
func RefreshTargetProfile(ctx context.Context, cfg Config, cookies Cookies, identity TargetIdentity, references []string, username string) (Profile, error) {
	return resolveTargetProfile(ctx, identity, references, username, func(ctx context.Context, lookup TargetIdentity) (Profile, error) {
		return lookupTargetProfile(ctx, cfg, cookies, lookup, nil)
	})
}

func lookupTargetProfile(ctx context.Context, cfg Config, cookies Cookies, identity TargetIdentity, setup func(context.Context) error) (Profile, error) {
	identity.profileOnly = true
	var profile Profile
	_, err := collectBound(ctx, cfg, cookies, identity.CurrentUsername, identity, 1, false, func(page Page) error {
		if page.Profile != nil {
			profile = *page.Profile
		}
		return nil
	}, setup)
	if err == nil && profile.ID == "" {
		err = errors.New("未能读取账号身份，请填写当前用户名后重试")
	}
	return profile, err
}

func resolveTargetProfile(ctx context.Context, identity TargetIdentity, references []string, username string, lookup func(context.Context, TargetIdentity) (Profile, error)) (Profile, error) {
	if !userIDPattern.MatchString(identity.UserID) {
		return Profile{}, fail("identity", "尚未保存可核对的数字用户 ID，无法确认新用户名属于原账号")
	}
	var attempts []TargetIdentity
	if strings.TrimSpace(username) != "" {
		name, err := Username(username)
		if err != nil {
			return Profile{}, err
		}
		attempts = append(attempts, TargetIdentity{UserID: identity.UserID, CurrentUsername: name})
	} else {
		seen := map[string]bool{}
		for _, id := range references {
			if userIDPattern.MatchString(id) && !seen[id] {
				attempts = append(attempts, TargetIdentity{UserID: identity.UserID, ReferencePostID: id})
				seen[id] = true
				if len(attempts) == 3 {
					break
				}
			}
		}
		if usernamePattern.MatchString(identity.CurrentUsername) {
			attempts = append(attempts, TargetIdentity{UserID: identity.UserID, CurrentUsername: identity.CurrentUsername})
		}
	}
	last := errors.New("没有可用于查找的历史推文，请填写当前用户名或主页地址")
	for _, attempt := range attempts {
		if err := ctx.Err(); err != nil {
			return Profile{}, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		p, err := lookup(attemptCtx, attempt)
		cancel()
		if ctx.Err() != nil {
			return Profile{}, ctx.Err()
		}
		if err == nil {
			if p.ID != identity.UserID {
				return Profile{}, fail("identity", "账号身份不一致：数字用户 ID 与原账号不符，未更新")
			}
			if !usernamePattern.MatchString(p.Username) {
				return Profile{}, fail("identity", "返回的用户名无效，未更新")
			}
			p.Username = strings.ToLower(p.Username)
			return p, nil
		}
		last = err
		switch FailureKind(err) {
		case "auth", "rate", "budget_storage":
			return Profile{}, err
		}
	}
	return Profile{}, fmt.Errorf("未能更新账号身份：%w", last)
}
