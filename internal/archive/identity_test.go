package archive

import (
	"encoding/json"
	"testing"
)

func TestTargetIdentityRenameAndTakeover(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SaveTargetProfile("alice", Profile{ID: "42", Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	post := Post{ID: "123", AuthorID: "42", Username: "alice", Raw: json.RawMessage(`{}`)}
	if err = s.SavePosts("alice", []Post{post}); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveTargetProfile("alice", Profile{ID: "42", Username: "newalice"}); err != nil {
		t.Fatal(err)
	}
	identity, err := s.TargetIdentity("alice")
	if err != nil || identity.UserID != "42" || identity.CurrentUsername != "newalice" || identity.ReferencePostID != "123" {
		t.Fatal(identity, err)
	}
	if err = s.SaveTargetProfile("alice", Profile{ID: "99", Username: "alice"}); FailureKind(err) != "identity" {
		t.Fatal("takeover accepted", err)
	}
	identity, _ = s.TargetIdentity("alice")
	if identity.UserID != "42" || identity.CurrentUsername != "newalice" {
		t.Fatal("binding changed", identity)
	}
}
func TestTargetIdentityRecoversHistoryAndRejectsAmbiguity(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, id := range []string{"42", "99"} {
		post := Post{ID: id, AuthorID: id, Username: "alice", Raw: json.RawMessage(`{}`)}
		if err = s.SavePosts("alice", []Post{post}); err != nil {
			t.Fatal(err)
		}
		identity, err := s.TargetIdentity("alice")
		if i == 0 && (err != nil || identity.UserID != "42") {
			t.Fatal(identity, err)
		}
		if i == 1 && FailureKind(err) != "identity" {
			t.Fatal("ambiguous history accepted", err)
		}
	}
}
func TestIdentityFromKnownPostOnly(t *testing.T) {
	data := []byte(`{"data":{"items":[{"rest_id":"88","core":{"user_results":{"result":{"rest_id":"99","legacy":{"screen_name":"other"}}}}},{"rest_id":"123","core":{"user_results":{"result":{"rest_id":"42","core":{"screen_name":"newalice"}}}}}]}}`)
	p, err := IdentityFromPost(data, "123")
	if err != nil || p.ID != "42" || p.Username != "newalice" {
		t.Fatal(p, err)
	}
	if _, err = IdentityFromPost(data, "124"); FailureKind(err) != "identity" {
		t.Fatal("missing reference accepted", err)
	}
}

func TestIdentityReferencePreservesAuthenticationFailures(t *testing.T) {
	for _, tc := range []struct{ body, kind string }{{`{"errors":[{"code":89}]}`, "auth"}, {`{"errors":[{"code":88}]}`, "rate"}, {`{"errors":[{"code":179}]}`, "access"}} {
		if _, err := IdentityFromPost([]byte(tc.body), "123"); FailureKind(err) != tc.kind {
			t.Fatal(err, tc.kind)
		}
	}
}
