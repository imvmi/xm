package archive

import (
	"encoding/json"
	"testing"
)

func TestReplyAuthorParsingAndLegacyBackfill(t *testing.T) {
	raw := []byte(`{"rest_id":"200","legacy":{"user_id_str":"42","in_reply_to_status_id_str":"100","in_reply_to_user_id_str":"42","full_text":"continued"},"core":{"user_results":{"result":{"rest_id":"42","core":{"screen_name":"newname"}}}}}`)
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	parsed, err := parsePost(data)
	if err != nil || parsed.ReplyToAuthorID != "42" || parsed.ReplyTo != "100" {
		t.Fatal(parsed, err)
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SavePosts("alice", []Post{{ID: "100", AuthorID: "42", Username: "oldname"}, parsed, {ID: "201", AuthorID: "42", Username: "newname", ReplyTo: "100", Raw: json.RawMessage(`{}`)}, {ID: "202", AuthorID: "42", Username: "newname", ReplyTo: "999", Raw: json.RawMessage(`invalid`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("ALTER TABLE posts DROP COLUMN reply_to_author_id"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = ensureReplyMetadata(s.DB); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{"100": "", "200": "42", "201": "42", "202": ""} {
		var got string
		if err = s.DB.QueryRow("SELECT reply_to_author_id FROM posts WHERE id=?", id).Scan(&got); err != nil || got != want {
			t.Fatal(id, got, want, err)
		}
	}
	// A later payload without reply-author metadata must not erase known identity.
	parsed.ReplyToAuthorID = ""
	parsed.Raw = json.RawMessage(`{}`)
	if err = s.SavePosts("alice", []Post{parsed}); err != nil {
		t.Fatal(err)
	}
	var got string
	if err = s.DB.QueryRow("SELECT reply_to_author_id FROM posts WHERE id='200'").Scan(&got); err != nil || got != "42" {
		t.Fatal(got, err)
	}
	parsed.ReplyTo = ""
	if err = s.SavePosts("alice", []Post{parsed}); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow("SELECT reply_to_author_id FROM posts WHERE id='200'").Scan(&got); err != nil || got != "" {
		t.Fatal(got, err)
	}
}
