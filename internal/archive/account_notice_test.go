package archive

import "testing"

func TestAccountNoticeDiagnosis(t *testing.T) {
	for _, tc := range []struct{ notice, reason string }{
		{"Account suspended", "suspended"},
		{"账号已被冻结", "suspended"},
		{"This account doesn’t exist", "not_found"},
		{"此账号不存在", "not_found"},
		{"These posts are protected", "protected"},
		{"这些帖子受到保护", "protected"},
		{"An article about Account suspended", ""},
		{"Something went wrong", ""},
		{"", ""},
	} {
		if err := accountNoticeFailure(tc.notice); FailureReason(err) != tc.reason {
			t.Fatalf("%q: %v", tc.notice, err)
		}
	}
	for _, tc := range []struct {
		path, current, target string
		want                  bool
	}{
		{"/Alice", "alice", "oldalice", true},
		{"/alice/with_replies", "alice", "oldalice", true},
		{"/oldalice", "alice", "oldalice", false},
		{"/alice/status/123", "alice", "alice", false},
		{"/other", "alice", "alice", false},
		{"/alice", "", "alice", true},
	} {
		if profilePathMatches(tc.path, tc.current, tc.target) != tc.want {
			t.Fatalf("path: %+v", tc)
		}
	}
}
