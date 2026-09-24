package api

import (
	"crypto/ed25519"
	"encoding/json"
	"html"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"exehub/internal/card"
	"exehub/internal/config"
	"exehub/internal/identity"
)

// the cases the Hub app's formatText is run against too
// (card/testdata/mentions.json): which mentions show, under which name,
// and the post's words with them set plain
func TestRenderPostMentions(t *testing.T) {
	raw, err := os.ReadFile("../card/testdata/mentions.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text, Plain string
		Names             map[string]string
		Mentions          []string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	link := regexp.MustCompile(`<a class="mention" href="/u/([0-9a-f]{16})">@(.*?)</a>`)
	for _, c := range cases {
		page := string(renderPost(c.Text, c.Names, webReading{}, nil))
		got := []string{}
		for _, m := range link.FindAllStringSubmatch(page, -1) {
			if c.Names[m[1]] != html.UnescapeString(m[2]) {
				t.Errorf("%s: %s links to %s", c.Name, m[2], m[1])
			}
			got = append(got, html.UnescapeString(m[2]))
		}
		if !reflect.DeepEqual(got, c.Mentions) {
			t.Errorf("%s: mentions %q, want %q\n%s", c.Name, got, c.Mentions, page)
		}
		if got := excerpt(card.NameMentions(c.Text, c.Names), 400); c.Plain != "" && got != c.Plain {
			t.Errorf("%s: plain %q, want %q", c.Name, got, c.Plain)
		}
	}
	// the markup itself
	names := map[string]string{"0123456789abcdef": "Tom & <Co>"}
	for _, c := range []struct{ in, want string }{
		{"hi @0123456789abcdef", `hi <a class="mention" href="/u/0123456789abcdef">@Tom &amp; &lt;Co&gt;</a>`},
		{"**@0123456789abcdef**", `<strong><a class="mention" href="/u/0123456789abcdef">@Tom &amp; &lt;Co&gt;</a></strong>`},
		{"`@0123456789abcdef`", "<code>@0123456789abcdef</code>"},
		{"a &@0123456789abcdef", `a &amp;<a class="mention" href="/u/0123456789abcdef">@Tom &amp; &lt;Co&gt;</a>`},
	} {
		if got := string(renderPost(c.in, names, webReading{}, nil)); got != c.want {
			t.Errorf("renderPost(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	// no names, no change: the page is renderText's
	if got, want := string(renderPost("hi @0123456789abcdef", names, webReading{Q: "?lang=ja"}, nil)), `hi <a class="mention" href="/u/0123456789abcdef?lang=ja">@Tom &amp; &lt;Co&gt;</a>`; got != want {
		t.Errorf("with ?lang=ja: got %s, want %s", got, want)
	}
	if got, want := renderPost("hi @0123456789abcdef", nil, webReading{}, nil), renderText("hi @0123456789abcdef", nil); got != want {
		t.Errorf("without names: %s", got)
	}
}

// a post keeps the id it was written with, and the feed, the page and
// the "@" list all say the name the profile goes by now
func TestMentionFollowsTheName(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	lpub, lpriv, _ := ed25519.GenerateKey(nil)
	jpub, jpriv, _ := ed25519.GenerateKey(nil)
	livid := identity.Fingerprint(lpub)
	ingest(t, s, lpriv, lpub, 1, "profile.set", map[string]any{"name": "Livid"})
	ingest(t, s, jpriv, jpub, 1, "profile.set", map[string]any{"name": "Joe"})
	ingest(t, s, jpriv, jpub, 2, "post.create", map[string]any{"text": "thanks @" + livid + " for this"})
	h := s.Handler()

	getJSON := func(path string, v any) {
		t.Helper()
		code, body := get(t, h, path)
		if code != 200 {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
		if err := json.Unmarshal([]byte(body), v); err != nil {
			t.Fatal(err)
		}
	}
	feed := func() (string, map[string]string) {
		t.Helper()
		var out struct {
			Posts []struct {
				Text     string            `json:"text"`
				Mentions map[string]string `json:"mentions"`
			} `json:"posts"`
		}
		getJSON("/v1/feed", &out)
		if len(out.Posts) != 1 {
			t.Fatalf("%d posts", len(out.Posts))
		}
		return out.Posts[0].Text, out.Posts[0].Mentions
	}
	text, names := feed()
	if text != "thanks @"+livid+" for this" || names[livid] != "Livid" {
		t.Fatalf("feed: %q %v", text, names)
	}
	_, page := get(t, h, "/")
	if want := `<a class="mention" href="/u/` + livid + `">@Livid</a>`; !strings.Contains(page, want) {
		t.Fatalf("page lacks %s", want)
	}

	ingest(t, s, lpriv, lpub, 2, "profile.set", map[string]any{"name": "Livid Two"})
	if _, names := feed(); names[livid] != "Livid Two" {
		t.Fatalf("after the rename: %v", names)
	}
	_, page = get(t, h, "/")
	if want := `">@Livid Two</a>`; !strings.Contains(page, want) {
		t.Fatalf("page lacks %s after the rename", want)
	}
	_, thread := get(t, h, "/p/"+func() string {
		var out struct {
			Posts []struct{ ID string } `json:"posts"`
		}
		getJSON("/v1/feed", &out)
		return out.Posts[0].ID
	}())
	if want := "thanks @Livid Two for this"; !strings.Contains(thread, want) {
		t.Fatalf("thread page's description lacks %q", want)
	}

	var found struct {
		Profiles []struct{ ID, Name string } `json:"profiles"`
	}
	getJSON("/v1/profiles?q=two", &found)
	if len(found.Profiles) != 1 || found.Profiles[0].ID != livid || found.Profiles[0].Name != "Livid Two" {
		t.Fatalf("q=two: %+v", found.Profiles)
	}
	getJSON("/v1/profiles?q=@"+livid[:6], &found)
	if len(found.Profiles) != 1 || found.Profiles[0].ID != livid {
		t.Fatalf("q=@id: %+v", found.Profiles)
	}
	// no q: whoever posted last comes first
	getJSON("/v1/profiles", &found)
	if len(found.Profiles) != 2 || found.Profiles[0].Name != "Joe" {
		t.Fatalf("no q: %+v", found.Profiles)
	}
	getJSON("/v1/profiles?q=%25", &found) // a percent sign is a character, not a wildcard
	if len(found.Profiles) != 0 {
		t.Fatalf("q=%%: %+v", found.Profiles)
	}
}
