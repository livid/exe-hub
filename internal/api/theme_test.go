package api

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	stats "github.com/livid/exe-stats"

	"exehub/internal/config"
)

var themedVar = regexp.MustCompile(`var\(--[tblsfgi]-[0-9a-z-]+\)`)

// themeSheets is each stylesheet the pages are drawn with, as written.
func themeSheets() map[string]string {
	cut := func(src, open string) string {
		a := strings.Index(src, open) + len(open)
		return src[a : a+strings.Index(src[a:], "</style>")]
	}
	page := cut(webHTML, "\n<style>\n")
	for _, act := range []string{"{{chrome}}", "{{popup}}", "{{schemevars}}"} {
		page = strings.ReplaceAll(page, act, "/* "+strings.Trim(act, "{}")+" */")
	}
	return map[string]string{"chrome.css": stats.ChromeCSS(), "popup.css": stats.PopupCSS(), "web.html": page,
		"the stats desk": cut(stats.TemplateHTML(), `{{define "statscss"}}<style>`)}
}

// The day is untouched: with each variable's own colour put back where
// the variable stands, a themed sheet is the sheet as it was written,
// to the byte, and a variable's day value is the colour it replaced.
func TestThemeDay(t *testing.T) {
	for name, src := range themeSheets() {
		th := newThemer()
		out := th.css(src)
		if len(th.tokens) == 0 {
			t.Errorf("%s: no colour found", name)
		}
		i := 0
		back := themedVar.ReplaceAllStringFunc(out, func(v string) string {
			if i >= len(th.tokens) {
				return v
			}
			tok := th.tokens[i]
			i++
			key := strings.TrimSuffix(strings.TrimPrefix(v, "var(--"), ")")
			lit := tok
			if m := themeVarRef.FindStringSubmatch(tok); m != nil {
				lit = themeRoot[m[1]]
			}
			if !strings.HasPrefix(key, "i-") && themeName(th.day[key]) != themeName(lit) {
				t.Errorf("%s: --%s is %s by day, and stands where %s stood", name, key, th.day[key], tok)
			}
			return tok
		})
		if i != len(th.tokens) || back != src {
			t.Errorf("%s: the sheet with its colours put back is not the sheet (%d of %d colours)", name, i, len(th.tokens))
		}
	}
}

// Every colour of the pages has its night, and the table holds no night
// for a colour no page uses: a rule written with a new colour, or in a
// new place, fails here by name until theme.go says what it is at night.
func TestThemeNight(t *testing.T) {
	th := webTheme.t
	var lost []string
	for k, where := range th.lost {
		lost = append(lost, "--"+k+" ("+th.day[k]+"), as in "+where)
	}
	sort.Strings(lost)
	for _, l := range lost {
		t.Errorf("no night value in theme.go for %s", l)
	}
	for _, s := range th.strange {
		t.Errorf("a colour in a property theme.go has no kind for: %s", s)
	}
	for k := range themeNight {
		if _, ok := th.day[k]; !ok {
			t.Errorf("theme.go keeps a night for --%s, which no sheet uses", k)
		}
	}
	// a glyph picture is inked for the night
	for k, v := range th.day {
		if strings.HasPrefix(k, "i-") && (!strings.Contains(v, `fill="black"`) || strings.Contains(th.night(k), `fill="black"`) || !strings.Contains(th.night(k), `fill="%23e2e2e2"`)) {
			t.Errorf("--%s: %s by night", k, th.night(k))
		}
	}
	if len(th.day) < 120 {
		t.Errorf("%d variables: the sheets were not read", len(th.day))
	}
}

// Appearance (PLAN.md): a page defines the variables three times — the
// day, the night for a dark system unless the reader chose Light, the
// night for a reader who chose Dark — says its choice on the root before
// the first paint, and carries the menu beside the language menu, in
// the reader's language. The replies frame takes no part.
func TestWebAppearance(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	h := s.Handler()
	_, body := get(t, h, "/")
	for _, want := range []string{
		`<meta name="color-scheme" content="light dark">`,
		`:root { color-scheme: light; `,
		`@media (prefers-color-scheme: dark) { :root:not([data-scheme="light"]):not(.framed) { color-scheme: dark; `,
		`:root[data-scheme="dark"] { color-scheme: dark; `,
		` --b-fff: #fff;`, ` --b-fff: #1c1c1c;`, ` --t-262626: #262626;`, ` --t-262626: #e2e2e2;`,
		`localStorage.getItem(KEY)`, `"exe-hub-scheme"`, `root.classList.add("sch")`,
		`border-color: var(--l-339); color: var(--t-339);`,
		`</span></span><span class="popup scheme" id="scheme">`,
		`<span class="popup scheme" id="scheme"><button type="button" aria-label="Appearance" title="Appearance" aria-haspopup="menu" aria-expanded="false" aria-controls="schmenu">`,
		`<span class="nm"><span class="li">Light</span><span class="da">Dark</span><span class="au">Auto</span></span>`,
		`<span class="pmenu icons" id="schmenu" role="menu" aria-label="Appearance" hidden>`,
		`role="menuitemradio" data-v="light"`, `role="menuitemradio" data-v="dark"`, `role="menuitemradio" data-v="auto"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the home page lacks %q", want)
		}
	}
	if n := strings.Count(body, `id="scheme"`); n != 1 {
		t.Errorf("%d Appearance buttons on the home page", n)
	}
	// no colour is left as a literal in a rule of the page's sheet but the
	// paper look's, which has a night of its own
	sheet := body[strings.Index(body, ":root[data-scheme=\"dark\"]"):strings.Index(body, "</style>")]
	sheet = sheet[strings.Index(sheet, "\n"):]
	if i := strings.Index(sheet, "color: #262626"); i >= 0 && !strings.Contains(sheet[max(0, i-300):i], ".paper") {
		t.Errorf("a literal ink is left in the sheet: %q", sheet[max(0, i-80):i+20])
	}
	// the three words in the reader's language
	_, zh := get(t, h, "/?lang=zh")
	_, ja := get(t, h, "/", "Accept-Language", "ja")
	if !strings.Contains(zh, `aria-label="外观"`) || !strings.Contains(zh, `<span class="li">浅色</span><span class="da">深色</span><span class="au">自动</span>`) ||
		!strings.Contains(ja, `<span class="li">ライト</span><span class="da">ダーク</span><span class="au">自動</span>`) {
		t.Error("the Appearance menu is not in the reader's language")
	}
	// the frame: no menu, no script, no meta of this page's; its sheet
	// keeps the day whatever the system
	unknown := strings.Repeat("ab", 32)
	_, frame := get(t, h, "/p/"+unknown+"/replies")
	if strings.Contains(frame, `id="scheme"`) || strings.Contains(frame, "exe-hub-scheme") || strings.Contains(frame, `<meta name="color-scheme"`) ||
		!strings.Contains(frame, `class="framed"`) || !strings.Contains(frame, `:not(.framed) { color-scheme: dark;`) {
		t.Error("the replies frame takes part in Appearance")
	}
}
