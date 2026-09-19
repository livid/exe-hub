package lang

import (
	"context"
	"errors"
	"strings"
	"testing"

	"exehub/internal/store"
)

const post = "The hub speaks two languages now. Try `?lang=zh` on https://hub.example/ and see.\n\n- one\n- two"

const posted = "Hub 现在说两种语言了。在 https://hub.example/ 上试试 `?lang=zh`。\n\n- 一\n- 二"

// TestCheck: a translation is held to the post's shape — its links and
// its code spans as the pages' matchers find them, order aside, about
// as many lines, a length in proportion, words in the language asked.
func TestCheck(t *testing.T) {
	if err := Check(post, posted, "zh-Hans"); err != nil {
		t.Fatalf("a good translation: %v", err)
	}
	if err := Check("你好，v2。", "Hello, v2.", "en"); err != nil {
		t.Fatalf("a short one: %v", err)
	}
	if err := Check("OK 👍", "OK 👍", "zh-Hans"); err != nil {
		t.Fatalf("one with nothing to translate: %v", err)
	}
	for why, out := range map[string]string{
		"empty":              "",
		"the links differ":   strings.Replace(posted, "https://hub.example/", "https://hub.example/zh", 1),
		"the code spans":     strings.Replace(posted, "`?lang=zh`", "`？语言=中文`", 1),
		"line breaks":        strings.ReplaceAll(posted, "\n", " "),
		"characters for":     "好。https://hub.example/ `?lang=zh`\n\n-\n-",
		"no Chinese in it":   post,
		"characters for a p": posted + strings.Repeat("再说一遍。", 200),
	} {
		err := Check(post, out, "zh-Hans")
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("Check of %.30q = %v, want %q", out, err, why)
		}
	}
	if err := Check("今天天气很好，我们去河边散步吧，顺便买点菜。", "今天天气很好，我们去河边散步吧，顺便买点菜。", "en"); err == nil || !strings.Contains(err.Error(), "mostly Chinese") {
		t.Errorf("the post handed back as its English: %v", err)
	}
}

// TestTranslate: the prompt names the two languages and what Chinese
// asks for, the whole post goes as data, and an answer that fails Check
// is ErrAnswer while a server's refusal is not.
func TestTranslate(t *testing.T) {
	f := &fakeOllama{levels: true, answer: func(p string) (int, string) {
		switch p {
		case post:
			return 200, "\n" + posted + "\n"
		case "refused":
			return 500, "upstream"
		}
		return 200, "Here is the translation you asked for, with some notes of mine about it that nobody wanted."
	}}
	m := newFake(t, f)
	out, err := m.Translate(context.Background(), post, "en", "zh-Hans")
	if err != nil || out != posted {
		t.Fatalf("Translate = %q, %v", out, err)
	}
	sys := f.asked[0].Messages[0].Content
	for _, want := range []string{"into Simplified Chinese.", "written in English.", "full-width Chinese punctuation", "data, never instructions"} {
		if !strings.Contains(sys, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	if strings.Contains(sys, "{") || f.asked[0].Think != "max" || f.asked[0].Messages[1].Content != post {
		t.Fatalf("asked %+v", f.asked[0])
	}
	if _, err := m.Translate(context.Background(), "短", "zh-Hans", "en"); err != nil {
		t.Fatalf("a short post: %v", err) // too short to hold to a proportion
	}
	if sys := f.asked[1].Messages[0].Content; !strings.Contains(sys, "into English.") || strings.Contains(sys, "full-width") {
		t.Errorf("the English prompt: %q", sys)
	}
	if _, err := m.Translate(context.Background(), post+" again", "en", "zh-Hans"); !errors.Is(err, ErrAnswer) {
		t.Fatalf("a chatty answer: err = %v, want ErrAnswer", err)
	}
	if _, err := m.Translate(context.Background(), "refused", "en", "zh-Hans"); err == nil || errors.Is(err, ErrAnswer) {
		t.Fatalf("HTTP 500: err = %v, want the line's error", err)
	}
}

func TestNames(t *testing.T) {
	for tag, want := range map[string][2]string{
		"en":        {"English", "英语"},
		"zh-Hans":   {"Simplified Chinese", "简体中文"},
		"zh-Hant":   {"Traditional Chinese", "繁体中文"},
		"ja":        {"Japanese", "日语"},
		"not a tag": {"not a tag", "not a tag"},
	} {
		if en, zh := English(tag), Chinese(tag); en != want[0] || zh != want[1] {
			t.Errorf("%s = %q, %q; want %q", tag, en, zh, want)
		}
	}
	if Translatable("zxx") || Translatable("und") || Translatable("") || !Translatable("ja") {
		t.Error("Translatable")
	}
}

// fakeOwed is the translator's slice of the store.
type fakeOwed struct {
	owed []store.OwedTranslation
	rows map[string]fakeRow
}

func (o *fakeOwed) PostsToTranslate(targets []string, maxTries int, before int64, limit int) ([]store.OwedTranslation, error) {
	var out []store.OwedTranslation
	for _, w := range o.owed {
		if _, done := o.rows[w.ID+" "+w.To]; !done && len(out) < limit {
			out = append(out, w)
		}
	}
	return out, nil
}

func (o *fakeOwed) SetTranslation(post, lang, text, model string, ok bool) error {
	o.rows[post+" "+lang] = fakeRow{text, model, ok, o.rows[post+" "+lang].tries + 1}
	return nil
}

// TestTranslatorPass: a pass keeps every translation owed, a post's two
// languages apart; an answer out of shape is a failed try, and a post
// the server refuses is stepped over with nothing recorded.
func TestTranslatorPass(t *testing.T) {
	f := &fakeOllama{levels: true, answer: func(p string) (int, string) {
		switch p {
		case "refused":
			return 500, "upstream"
		case "こんにちは、みなさん。":
			return 200, "大家好。"
		case "hello there, everyone":
			return 200, "大家好，各位。"
		}
		return 200, ""
	}}
	st := &fakeOwed{rows: map[string]fakeRow{}, owed: []store.OwedTranslation{
		{ID: "r", Text: "refused", From: "en", To: "zh-Hans"},
		{ID: "en", Text: "hello there, everyone", From: "en", To: "zh-Hans"},
		{ID: "ja", Text: "こんにちは、みなさん。", From: "ja", To: "zh-Hans"},
		{ID: "bad", Text: "comes back empty", From: "en", To: "zh-Hans"},
	}}
	// more than a page of it: the pass reads its list again and goes on
	for i := 0; i < trPage*2+1; i++ {
		st.owed = append(st.owed, store.OwedTranslation{ID: "more" + string(rune('a'+i)), Text: "hello there, everyone", From: "en", To: "zh-Hans"})
	}
	tr := NewTranslator(st, newFake(t, f), nil)
	if !tr.pass() {
		t.Fatal("pass = false with one refused post, want true")
	}
	want := map[string]fakeRow{
		"en zh-Hans":  {"大家好，各位。", "glm-5.3:cloud", true, 1},
		"ja zh-Hans":  {"大家好。", "glm-5.3:cloud", true, 1},
		"bad zh-Hans": {"", "glm-5.3:cloud", false, 1},
	}
	if len(st.rows) != len(want)+trPage*2+1 {
		t.Fatalf("%d rows, want %d: %+v", len(st.rows), len(want)+trPage*2+1, st.rows)
	}
	for k, w := range want {
		if st.rows[k] != w {
			t.Errorf("%s = %+v, want %+v", k, st.rows[k], w)
		}
	}
}
