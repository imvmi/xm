package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
)

type Asset struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

type Post struct {
	ID        string          `json:"id"`
	AuthorID  string          `json:"author_id"`
	Username  string          `json:"username"`
	Text      string          `json:"text"`
	CreatedAt string          `json:"created_at"`
	URL       string          `json:"url"`
	ReplyTo   string          `json:"reply_to,omitempty"`
	QuoteID   string          `json:"quote_id,omitempty"`
	RepostID  string          `json:"repost_id,omitempty"`
	Assets    []Asset         `json:"assets"`
	Quote     *Post           `json:"quote,omitempty"`
	Raw       json.RawMessage `json:"raw"`
}

type Profile struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Protected bool   `json:"protected"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

type Page struct {
	Profile     *Profile
	Posts       []Post
	Timeline    bool
	Unavailable int
}
type object = map[string]any

func obj(v any) object { m, _ := v.(map[string]any); return m }
func at(v any, path ...string) any {
	for _, k := range path {
		v = obj(v)[k]
	}
	return v
}
func str(v any) string   { s, _ := v.(string); return s }
func array(v any) []any  { a, _ := v.([]any); return a }
func boolean(v any) bool { b, _ := v.(bool); return b }
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func ParsePage(data []byte, operation string) (Page, error) {
	var root object
	var page Page
	if json.Unmarshal(data, &root) != nil {
		return page, errors.New("X 返回的响应不是有效 JSON，可能需要登录验证")
	}
	if errs := array(root["errors"]); len(errs) > 0 {
		code, _ := at(errs[0], "code").(float64)
		switch int(code) {
		case 32, 89, 215, 239, 353:
			return page, fail("auth", "Cookie 无效或登录态失效，请在浏览器确认登录后重新导入")
		case 88:
			return page, fail("rate", "X 请求限流，请等待后重试")
		case 179:
			return page, fail("access", "当前账号无权查看该锁推账号的内容")
		case 64, 326:
			return page, fail("auth", "采集账号受限或需要人工验证，请在浏览器处理")
		default:
			return page, fmt.Errorf("X 返回接口错误（code=%d）；本次采集未完整完成", int(code))
		}
	}
	u := at(root, "data", "user", "result")
	if u == nil {
		return page, errors.New("未找到 X 用户数据，可能无权访问或页面响应结构已变化")
	}
	if str(at(u, "__typename")) == "UserUnavailable" {
		return page, fail("access", "目标账号不可用或当前账号无权访问")
	}
	if operation == "UserByScreenName" || operation == "UserByRestId" {
		p := &Profile{
			ID:        str(at(u, "rest_id")),
			Username:  first(str(at(u, "core", "screen_name")), str(at(u, "legacy", "screen_name"))),
			Protected: boolean(at(u, "legacy", "protected")) || boolean(at(u, "privacy", "protected")),
			AvatarURL: first(str(at(u, "avatar", "image_url")), str(at(u, "core", "avatar_image_url")), str(at(u, "legacy", "profile_image_url_https"))),
		}
		if p.ID == "" {
			return page, errors.New("用户资料缺少 ID，响应结构可能已变化")
		}
		page.Profile = p
		return page, nil
	}
	var instructions []any
	// Match only the requested user's timeline, never recommendations or quoted users.
	for _, path := range [][]string{{"timeline_v2", "timeline", "instructions"}, {"timeline", "timeline", "instructions"}, {"timeline", "instructions"}} {
		if v := at(u, path...); v != nil {
			instructions = array(v)
			page.Timeline = true
			break
		}
	}
	if !page.Timeline {
		return page, errors.New("未找到用户时间线，可能无权访问或响应结构已变化")
	}
	seen := make(map[string]bool)
	var parseErr error
	var item func(any)
	item = func(content any) {
		if content == nil {
			return
		}
		if r := at(content, "tweet_results", "result"); r != nil {
			if wrapped := at(r, "tweet"); wrapped != nil {
				r = wrapped
			}
			typ := str(at(r, "__typename"))
			if typ == "TweetUnavailable" || typ == "TweetTombstone" {
				page.Unavailable++
				return
			}
			p, err := parsePost(r)
			if err != nil {
				parseErr = errors.New("部分推文未能解析，本次采集不完整；响应结构可能发生变化")
				return
			}
			if err == nil && !seen[p.ID] {
				page.Posts = append(page.Posts, p)
				seen[p.ID] = true
			}
		}
		if strings.Contains(str(at(content, "itemType")), "Tombstone") {
			page.Unavailable++
		}
		for _, key := range []string{"itemContent", "item"} {
			if child := at(content, key); child != nil {
				item(child)
			}
		}
		for _, child := range array(at(content, "items")) {
			item(child)
		}
	}
	for _, instruction := range instructions {
		for _, entry := range array(at(instruction, "entries")) {
			item(at(entry, "content"))
		}
		if e := at(instruction, "entry"); e != nil {
			item(at(e, "content"))
		}
		for _, entry := range array(at(instruction, "moduleItems")) {
			item(entry)
		}
	}
	if parseErr != nil {
		return page, parseErr
	}
	// A populated response with unrecognized tweet objects must not masquerade as empty.
	if len(page.Posts) == 0 && page.Unavailable == 0 && strings.Contains(string(data), `"tweet_results"`) {
		return page, errors.New("时间线包含未能解析的推文，采集器需要适配新的响应结构")
	}
	return page, nil
}

const MaxQuoteDepth = 4

func parsePost(v any) (Post, error) { return parsePostDepth(v, 0) }

func parsePostDepth(v any, depth int) (Post, error) {
	if wrapped := at(v, "tweet"); wrapped != nil {
		v = wrapped
	}
	var p Post
	p.ID = first(str(at(v, "rest_id")), str(at(v, "legacy", "id_str")))
	legacy := at(v, "legacy")
	if p.ID == "" || legacy == nil {
		return p, errors.New("invalid tweet")
	}
	u := at(v, "core", "user_results", "result")
	p.AuthorID = first(str(at(u, "rest_id")), str(at(legacy, "user_id_str")))
	p.Username = first(str(at(u, "core", "screen_name")), str(at(u, "legacy", "screen_name")))
	if p.AuthorID == "" || p.Username == "" {
		return p, errors.New("missing tweet author")
	}
	p.CreatedAt = str(at(legacy, "created_at"))
	p.Text = html.UnescapeString(first(str(at(v, "note_tweet", "note_tweet_results", "result", "text")), str(at(legacy, "full_text"))))
	p.ReplyTo = str(at(legacy, "in_reply_to_status_id_str"))
	p.QuoteID = str(at(legacy, "quoted_status_id_str"))
	p.URL = "https://x.com/i/status/" + p.ID
	mediaLegacy := legacy
	quoteSource := v
	if rt := at(legacy, "retweeted_status_result", "result"); rt != nil {
		if wrapped := at(rt, "tweet"); wrapped != nil {
			rt = wrapped
		}
		p.RepostID = str(at(rt, "rest_id"))
		mediaLegacy = at(rt, "legacy")
		quoteSource = rt
		p.QuoteID = str(at(mediaLegacy, "quoted_status_id_str"))
		if full := first(str(at(rt, "note_tweet", "note_tweet_results", "result", "text")), str(at(mediaLegacy, "full_text"))); full != "" {
			p.Text = "RT: " + html.UnescapeString(full)
		}
	}
	media := array(at(mediaLegacy, "extended_entities", "media"))
	if len(media) == 0 {
		media = array(at(mediaLegacy, "entities", "media"))
	}
	for _, m := range media {
		a := Asset{ID: first(str(at(m, "media_key")), str(at(m, "id_str"))), Kind: str(at(m, "type"))}
		switch a.Kind {
		case "photo":
			a.URL = str(at(m, "media_url_https"))
			if u, err := url.Parse(a.URL); err == nil && u.Host != "" {
				q := u.Query()
				q.Set("name", "orig")
				u.RawQuery = q.Encode()
				a.URL = u.String()
			}
		case "video", "animated_gif":
			best := float64(-1)
			for _, variant := range array(at(m, "video_info", "variants")) {
				rate, _ := at(variant, "bitrate").(float64)
				if str(at(variant, "content_type")) == "video/mp4" && rate > best {
					best = rate
					a.URL = str(at(variant, "url"))
				}
			}
		default:
			continue
		}
		if a.ID == "" {
			sum := sha256.Sum256([]byte(first(str(at(m, "media_url_https")), a.URL)))
			a.ID = hex.EncodeToString(sum[:16])
		}
		p.Assets = append(p.Assets, a)
	}
	if quoted := at(quoteSource, "quoted_status_result", "result"); quoted != nil {
		if wrapped := at(quoted, "tweet"); wrapped != nil {
			quoted = wrapped
		}
		p.QuoteID = first(p.QuoteID, str(at(quoted, "rest_id")), str(at(quoted, "legacy", "id_str")))
		if depth < MaxQuoteDepth && str(at(quoted, "__typename")) != "TweetUnavailable" && str(at(quoted, "__typename")) != "TweetTombstone" {
			if q, err := parsePostDepth(quoted, depth+1); err == nil && q.ID != p.ID {
				p.QuoteID, p.Quote = q.ID, &q
			}
		}
	}
	p.Raw, _ = json.Marshal(v)
	return p, nil
}

// AuthorDetails reads only the author metadata already included in a saved post.
func AuthorDetails(raw []byte) (name, bio string) {
	var v object
	if json.Unmarshal(raw, &v) != nil {
		return "", ""
	}
	u := at(v, "core", "user_results", "result")
	return first(str(at(u, "core", "name")), str(at(u, "legacy", "name"))), str(at(u, "legacy", "description"))
}
