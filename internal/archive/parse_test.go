package archive

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTimelineLongTextMediaAndModules(t *testing.T) {
	p, err := ParsePage(fixture(t, "timeline.json"), "UserTweets")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Timeline || len(p.Posts) != 3 || p.Unavailable != 1 {
		t.Fatalf("wrong timeline: %+v", p)
	}
	post := p.Posts[0]
	if post.Text != "完整的长推文 & 中文" || post.ID != "1900000000000000001" {
		t.Fatalf("truncated text or id: %+v", post)
	}
	if len(post.Assets) != 2 || !strings.Contains(post.Assets[0].URL, "name=orig") || post.Assets[1].URL != "https://video.twimg.com/high.mp4" {
		t.Fatalf("wrong media: %+v", post.Assets)
	}
	if post.QuoteID != "1899999999999999999" {
		t.Fatal("quote reference lost")
	}
	for _, post := range p.Posts {
		if post.ID == post.QuoteID {
			t.Fatal("quoted content independently archived")
		}
	}
	if p.Posts[2].RepostID != "1800000000000000000" || p.Posts[2].Text != "RT: 原始推文完整内容" {
		t.Fatal("repost lost")
	}
	if !json.Valid(post.Raw) {
		t.Fatal("invalid raw JSON")
	}
}

func TestProtectedProfileAndErrors(t *testing.T) {
	p, err := ParsePage([]byte(`{"data":{"user":{"result":{"rest_id":"42","core":{"screen_name":"alice"},"privacy":{"protected":true},"avatar":{"image_url":"https://pbs.twimg.com/profile_images/42/alice_normal.jpg"}}}}}`), "UserByScreenName")
	if err != nil || p.Profile == nil || !p.Profile.Protected || p.Profile.AvatarURL != "https://pbs.twimg.com/profile_images/42/alice_normal.jpg" {
		t.Fatal("protected profile not detected", err)
	}
	for _, b := range []string{`{"errors":[{"code":179}]}`, `{"errors":[{"code":89}]}`, `{"errors":[{"code":88}]}`, `{"data":{"user":{"result":{"__typename":"UserUnavailable"}}}}`, `{"data":{"user":{"result":{"timeline":{"unknown":[]}}}}}`} {
		if _, err := ParsePage([]byte(b), "UserTweets"); err == nil {
			t.Fatal("must not treat inaccessible or changed schema as empty:", b)
		}
	}
}

func TestUsername(t *testing.T) {
	for _, s := range []string{"Alice", "@alice", "https://x.com/alice", "https://twitter.com/alice/"} {
		n, e := Username(s)
		if e != nil || n != "alice" {
			t.Fatal(s, n, e)
		}
	}
	for _, s := range []string{"../data", "https://x.com.evil/alice", "https://x.com/alice/status/1", "", "bad-name", "https://u:p@x.com/alice"} {
		if _, e := Username(s); e == nil {
			t.Fatal("invalid target accepted", s)
		}
	}
}
