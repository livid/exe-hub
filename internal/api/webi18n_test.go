package api

import (
	"regexp"
	"sort"
	"testing"
)

// TestWebStrings: the three columns have the same keys, each string
// takes the same values as its English, and a plural form has a base.
func TestWebStrings(t *testing.T) {
	en := webStrings["en"]
	holes := regexp.MustCompile(`\{(\w+)\}`)
	slots := func(s string) string {
		m := holes.FindAllStringSubmatch(s, -1)
		out := make([]string, 0, len(m))
		for _, x := range m {
			out = append(out, x[1])
		}
		sort.Strings(out)
		return "[" + joinStrings(out, " ") + "]"
	}
	for code, table := range webStrings {
		if code == "en" {
			continue
		}
		for k, v := range en {
			if _, ok := table[k]; !ok {
				if _, isOne := en[k[:len(k)-len(".one")]]; len(k) > 4 && k[len(k)-4:] == ".one" && isOne {
					continue // English inflects; the others may not
				}
				t.Errorf("%s lacks %q", code, k)
			} else if s, want := slots(table[k]), slots(v); s != want {
				t.Errorf("%s %q takes %s, English %s", code, k, s, want)
			}
		}
		for k := range table {
			if _, ok := en[k]; !ok {
				t.Errorf("%s has %q, English does not", code, k)
			}
		}
	}
	for code, table := range webStrings {
		for k, v := range table {
			if v == "" && k != "since.post" && k != "replying.post" && k != "replying.pre" {
				t.Errorf("%s %q is empty", code, k)
			}
			if len(k) > 4 && k[len(k)-4:] == ".one" {
				if _, ok := table[k[:len(k)-4]]; !ok {
					t.Errorf("%s %q has no base form", code, k)
				}
			}
		}
	}
	if len(webLocales) != len(webStrings) || len(webTmpls) != len(webStrings) {
		t.Errorf("%d locales, %d templates for %d tables", len(webLocales), len(webTmpls), len(webStrings))
	}
}

func joinStrings(a []string, sep string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// TestWebT: a value fills its slot, a count of one picks the singular
// where the language has one, and every language keeps its number
// where the template puts it.
func TestWebT(t *testing.T) {
	en, zh, ja := webLocales["en"], webLocales["zh"], webLocales["ja"]
	for _, c := range []struct {
		got, want string
	}{
		{en.T("replies", "n", 1), "1 reply"}, {en.T("replies", "n", 2), "2 replies"}, {en.T("replies", "n", 0), "0 replies"},
		{zh.T("replies", "n", 1), "1 条回复"}, {ja.T("replies", "n", 1), "1 件の返信"},
		{en.T("matches", "n", 1), "1 post matches"}, {en.T("matches", "n", 12), "12 posts match"},
		{en.T("title.on", "name", "Ann", "host", "hub.example"), "Ann on hub.example"},
		{zh.T("title.on", "name", "Ann", "host", "hub.example"), "Ann · hub.example"},
		{ja.T("title.on", "name", "Ann", "host", "hub.example"), "hub.example の Ann"},
		{en.T("inreply.name", "name", "Bo"), "in reply to Bo"}, {ja.T("inreply.name", "name", "Bo"), "Bo への返信"},
		{en.T("tr.from", "lang", "Chinese"), "Translated from Chinese"}, {ja.T("tr.from", "lang", "英語"), "英語から翻訳"},
		{en.T("nothing"), "Nothing here yet."}, {en.T("no.such.key"), "no.such.key"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	d := &webData{L: ja, DateLoc: "ja"}
	if js := d.JS(); js["loc"] != "ja" || js["s"].(map[string]string)["ok"] != "OK" {
		t.Errorf("JS() = %v", js)
	}
}
