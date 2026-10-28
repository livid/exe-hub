package emoji

import (
	"encoding/json"
	"testing"
)

// TestIs: one fully-qualified emoji of Unicode's, exactly as it spells
// it, and nothing else.
func TestIs(t *testing.T) {
	for s, want := range map[string]bool{
		"🎉": true, "❤️": true, "👍🏽": true, "🇯🇵": true, "1️⃣": true, "👩‍💻": true, "🦄": true,
		"🫩":  true,  // Unicode 16.0: face with bags under eyes
		"❤":  false, // without its U+FE0F
		"1⃣": false, // the keycap without it
		"🏽":  false, // a skin tone alone is a component
		"🎉🎉": false, "": false, "BUY": false, "a": false, "1": false, " 🎉": false,
	} {
		if Is(s) != want {
			t.Errorf("Is(%q) = %v, want %v", s, !want, want)
		}
	}
	if len(All) < 3700 || All[0] != "😀" {
		t.Errorf("All: %d, first %q", len(All), All[0])
	}
}

// TestNames: every emoji has a name in every language, in All's order,
// and the version follows the table.
func TestNames(t *testing.T) {
	for _, lang := range Langs {
		raw, ver := Names(lang)
		var rows [][]string
		if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != len(All) || len(ver) != 12 {
			t.Fatalf("%s: %d rows for %d emoji, version %q, %v", lang, len(rows), len(All), ver, err)
		}
		for i, r := range rows {
			if r[0] != All[i] || r[1] == "" {
				t.Fatalf("%s row %d: %q, want %q with a name", lang, i, r, All[i])
			}
		}
	}
	for _, c := range []struct{ lang, e, want string }{
		{"en", "😂", "Face with Tears of Joy"}, {"en", "👍🏽", "Thumbs Up: Medium Skin Tone"}, {"en", "🇯🇵", "Flag: Japan"},
		{"zh", "🎉", "拉炮彩带"}, {"ja", "🚀", "ロケット"}, {"zh-hant", "🦉", "貓頭鷹"},
		{"fr", "🎉", "Party Popper"}, // no table: English
		{"en", "BUY", ""},
	} {
		if got := Name(c.lang, c.e); got != c.want {
			t.Errorf("Name(%s, %s) = %q, want %q", c.lang, c.e, got, c.want)
		}
	}
	if raw, ver := Names("fr"); raw != nil || ver != "" {
		t.Error("a table for a language there is none for")
	}
}
