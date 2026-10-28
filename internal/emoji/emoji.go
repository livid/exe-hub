// Package emoji is every emoji a post.react may give, and their names
// (PLAN.md, Reactions — Which emoji): Unicode 16.0's fully-qualified
// sequences, the version the preview pictures were cut from, named by
// CLDR 46 in the languages the pages speak. genemoji.py writes the
// data beside this file; nothing in it is edited by hand. Both sources
// are Unicode's, under the Unicode License v3
// (https://www.unicode.org/license.txt).
package emoji

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
)

//go:embed emoji.txt
var list string

//go:embed names/*.json
var namesFS embed.FS

// All is every emoji in Unicode's order (emoji-test.txt's: smileys
// first, flags last).
var All = strings.Split(strings.TrimSuffix(list, "\n"), "\n")

var set = func() map[string]bool {
	m := make(map[string]bool, len(All))
	for _, e := range All {
		m[e] = true
	}
	return m
}()

// Is reports whether s is exactly one fully-qualified emoji: "❤️" with
// its U+FE0F, "👍🏽", "🇯🇵", "1️⃣" — not "❤" bare, not two, not words.
func Is(s string) bool { return set[s] }

// Langs are the languages the names come in: the pages' own codes.
var Langs = []string{"en", "zh", "zh-hant", "ja"}

type table struct {
	raw  []byte
	ver  string
	name map[string]string
}

var (
	once   sync.Once
	tables map[string]*table
)

func load() {
	tables = map[string]*table{}
	for _, lang := range Langs {
		raw, err := namesFS.ReadFile("names/" + lang + ".json")
		if err != nil {
			panic("emoji: " + err.Error()) // embedded: a build without it is broken
		}
		var rows [][]string
		if err := json.Unmarshal(raw, &rows); err != nil {
			panic("emoji: names/" + lang + ".json: " + err.Error())
		}
		t := &table{raw: raw, name: make(map[string]string, len(rows))}
		for _, r := range rows {
			if len(r) >= 2 {
				t.name[r[0]] = r[1]
			}
		}
		sum := sha256.Sum256(raw)
		t.ver = hex.EncodeToString(sum[:6])
		tables[lang] = t
	}
}

// Names is the table for lang as the pages' script reads it — [[emoji,
// name, keywords?], …] in All's order — and a version naming its
// contents, for an address a browser may keep for good; nil for a
// language there is none for.
func Names(lang string) (raw []byte, ver string) {
	once.Do(load)
	if t := tables[lang]; t != nil {
		return t.raw, t.ver
	}
	return nil, ""
}

// Name is e's name in lang, the English one where lang has no table,
// "" for no emoji.
func Name(lang, e string) string {
	once.Do(load)
	t := tables[lang]
	if t == nil {
		t = tables["en"]
	}
	return t.name[e]
}
