package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestKnownTargetPostIDsChecksFullArchiveInChunks(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	posts := make([]Post, 5001)
	ids := make([]string, len(posts))
	for i := range posts {
		ids[i] = fmt.Sprint(i + 1)
		posts[i] = Post{ID: ids[i], AuthorID: "42", Username: "alice"}
	}
	if err = s.SavePosts("alice", posts); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePosts("bob", []Post{{ID: "9999", AuthorID: "99", Username: "bob"}}); err != nil {
		t.Fatal(err)
	}
	known, err := s.KnownTargetPostIDs(context.Background(), "alice", append(ids, "9999", "missing", "1"))
	if err != nil || len(known) != len(posts) || !known["1"] || !known["5001"] || known["9999"] || known["missing"] {
		t.Fatalf("full archive lookup: count=%d err=%v", len(known), err)
	}
	known, err = s.KnownTargetPostIDs(context.Background(), "alice", nil)
	if err != nil || len(known) != 0 {
		t.Fatal(known, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.KnownTargetPostIDs(ctx, "alice", ids[:1]); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.KnownTargetPostIDs(context.Background(), "alice", ids[:1]); err == nil {
		t.Fatal("failed lookup silently treated archived posts as new")
	}
}

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

func TestKnownTargetPostsAreBoundedAndTargetScoped(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"100", "101", "102", "103"} {
		if err = s.SavePosts("alice", []Post{{ID: id, AuthorID: "42", Username: "alice"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.SavePosts("bob", []Post{{ID: "999", AuthorID: "99", Username: "bob"}}); err != nil {
		t.Fatal(err)
	}
	known, err := s.KnownTargetPosts("alice", 2)
	if err != nil || len(known) != 2 || !known["103"] || !known["102"] || known["999"] {
		t.Fatal("incorrect incremental boundary", known, err)
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
