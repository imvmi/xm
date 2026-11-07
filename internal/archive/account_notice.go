package archive

import "strings"

// Only inspect first-party empty-state headings on the requested profile page.
// A missing username is not proof that the original numeric identity was deleted.
func accountNoticeFailure(notice string) error {
	for _, line := range strings.Split(notice, "\n") {
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "account suspended", "账号已被冻结", "账号被冻结", "帳戶已停用":
			return accountFailure("suspended", AccountSuspendedMessage)
		case "this account doesn’t exist", "this account doesn't exist", "此账号不存在", "该账号不存在", "這個帳戶不存在":
			return accountFailure("not_found", AccountNotFoundMessage)
		case "these posts are protected", "these tweets are protected", "这些帖子受到保护", "这些推文受到保护":
			return accountFailure("protected", AccountProtectedMessage)
		}
	}
	return nil
}

func profilePathMatches(path, currentUsername, target string) bool {
	name := currentUsername
	if name == "" {
		name = target
	}
	path = strings.TrimSuffix(path, "/")
	return strings.EqualFold(path, "/"+name) || strings.EqualFold(path, "/"+name+"/with_replies")
}
