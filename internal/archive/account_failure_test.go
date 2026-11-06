package archive

import "testing"

func TestAccountFailureDiagnosis(t *testing.T) {
	for _, tc := range []struct{ body, op, kind, reason string }{
		{`{"data":{"user":{"result":{"__typename":"UserUnavailable","reason":"Suspended"}}}}`, "UserByScreenName", "access", "suspended"},
		{`{"data":{"user":{"result":{"__typename":"UserUnavailable","reason":"NotFound"}}}}`, "UserByScreenName", "access", "not_found"},
		{`{"data":{"user":{"result":{"__typename":"UserUnavailable","reason":"Unknown"}}}}`, "UserTweets", "access", "unavailable"},
		{`{"errors":[{"code":63}]}`, "UserByScreenName", "access", "suspended"},
		{`{"errors":[{"code":50}]}`, "UserByScreenName", "access", "not_found"},
		{`{"errors":[{"code":64}]}`, "UserByScreenName", "auth", ""},
		{`{"errors":[{"code":179}]}`, "UserTweets", "access", "protected"},
		{`{"data":{"user":null}}`, "UserByScreenName", "access", "username_missing"},
		{`{"data":{"user":{"result":null}}}`, "UserByScreenName", "access", "username_missing"},
		{`{"data":{"user":null}}`, "UserTweets", "transient", ""},
		{`{"data":{}}`, "UserByScreenName", "transient", ""},
		{`{"data":{"user":{}}}`, "UserByScreenName", "transient", ""},
	} {
		t.Run(tc.op+tc.body, func(t *testing.T) {
			_, err := ParsePage([]byte(tc.body), tc.op)
			if err == nil || FailureKind(err) != tc.kind || FailureReason(err) != tc.reason {
				t.Fatalf("kind=%s reason=%s err=%v", FailureKind(err), FailureReason(err), err)
			}
		})
	}
}
