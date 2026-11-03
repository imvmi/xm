package archive

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestResolveTargetProfile(t *testing.T) {
	identity := TargetIdentity{UserID: "42", CurrentUsername: "oldname"}
	var attempts []string
	p, err := resolveTargetProfile(context.Background(), identity, []string{"300", "300", "200", "100", "99"}, "", func(_ context.Context, i TargetIdentity) (Profile, error) {
		attempts = append(attempts, i.ReferencePostID)
		if i.UserID != "42" || i.CurrentUsername != "" {
			t.Fatal(i)
		}
		if i.ReferencePostID == "300" {
			return Profile{}, errors.New("post deleted")
		}
		return Profile{ID: "42", Username: "NewName"}, nil
	})
	if err != nil || p.Username != "newname" || !reflect.DeepEqual(attempts, []string{"300", "200"}) {
		t.Fatal(p, attempts, err)
	}
	for _, kind := range []string{"auth", "rate"} {
		calls := 0
		_, err = resolveTargetProfile(context.Background(), identity, []string{"300", "200"}, "", func(context.Context, TargetIdentity) (Profile, error) { calls++; return Profile{}, fail(kind, "stop") })
		if calls != 1 || FailureKind(err) != kind {
			t.Fatal(kind, calls, err)
		}
	}
	calls := 0
	_, err = resolveTargetProfile(context.Background(), identity, []string{"300", "200", "100", "99"}, "", func(context.Context, TargetIdentity) (Profile, error) {
		calls++
		return Profile{}, errors.New("unavailable")
	})
	if calls != 4 || err == nil {
		t.Fatal("unbounded automatic attempts", calls, err)
	}
	for _, id := range []string{"42", "99"} {
		p, err = resolveTargetProfile(context.Background(), identity, []string{"300"}, "https://x.com/newname", func(_ context.Context, i TargetIdentity) (Profile, error) {
			if i.CurrentUsername != "newname" || i.ReferencePostID != "" {
				t.Fatal(i)
			}
			return Profile{ID: id, Username: "newname"}, nil
		})
		if (err == nil) != (id == "42") {
			t.Fatal("manual identity check", p, err)
		}
	}
	for _, tc := range []struct {
		identity TargetIdentity
		username string
	}{{TargetIdentity{}, "newname"}, {identity, "https://example.com/newname"}} {
		_, err = resolveTargetProfile(context.Background(), tc.identity, nil, tc.username, func(context.Context, TargetIdentity) (Profile, error) {
			t.Fatal("invalid input reached browser")
			return Profile{}, nil
		})
		if err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = resolveTargetProfile(ctx, identity, nil, "newname", func(context.Context, TargetIdentity) (Profile, error) {
		t.Fatal("cancelled lookup started")
		return Profile{}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestIdentityReferencePosts(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SavePosts("alice", []Post{{ID: "9", AuthorID: "42"}, {ID: "10", AuthorID: "42"}, {ID: "100", AuthorID: "42"}, {ID: "1000", AuthorID: "42"}, {ID: "9999", AuthorID: "99"}}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.IdentityReferencePosts("alice", "42")
	if err != nil || !reflect.DeepEqual(ids, []string{"1000", "100", "10"}) {
		t.Fatal(ids, err)
	}
}
