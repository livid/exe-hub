package api

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	stats "github.com/livid/exe-stats"
)

// The pages by night (PLAN.md, Appearance). The stylesheets the pages are
// drawn with — the chrome and the pop-up button exe-stats gives, the
// template's own, the stats desk's — say their colours as Platinum says
// them, #ccc and #262626, and stay written that way. As the hub starts,
// theme reads each of them once and puts a variable where every colour
// stands, and the page's head defines the variables twice: the day's
// values, which are the stylesheet's own words, and the night's, from
// the table below. A reader whose system is dark gets the night's
// unless the Appearance menu says Light, and anyone gets them when it
// says Dark (web.html, the head script and the "scheme" template).
//
// A variable is named for the job the colour does and for the colour:
// --t-666 is #666 as text, --b-fff white as a ground. The job is in the
// name because one Platinum grey does several, and each wants another
// value by night: #999 is a faint word (lighter by night), the shadow
// edge of a bevel (darker), and a hairline round a picture (lighter
// again). The kinds, by the property a colour is found in:
//
//	t  text: color and its kin
//	b  a ground: background
//	l  a line: border, outline
//	s  a bevel or a shadow: box-shadow
//	f  a pixel glyph's fill
//	g  a stop of a gradient: the title bar's stripes, the close box
//	i  a glyph drawn in black inside a data: picture
//
// and after the colour, where the selector says the colour stands
// somewhere particular, what it says (themeWhere): -p a button held
// down, -ink a black line on the white frame, and a few more. A colour
// with no value in the table keeps its day value by night, and
// TestThemeNight names it: a new rule's colours get their night in the
// commit that writes the rule.
//
// Nothing here changes the day: every variable's day value is the
// literal it replaced (TestThemeDay puts them back and compares).
var themeNight = map[string]string{
	"b-000": "#000", "b-262626": "#e2e2e2", "b-262626-p": "#e2e2e2", "b-339": "#3d3dad", "b-339-l4": "#6f6fe0",
	"b-666": "#202020", "b-666-p": "#202020", "b-66c-l3": "#5050b5",
	"b-888-p": "#242424", "b-99f": "#6a6ad8", "b-99f-l2": "#3b3b88",
	"b-a02525": "#8a1f1f", "b-aaa": "#2a2a2a", "b-adadad": "#171717", "b-b3b3b3": "#1a1a1a", "b-bbb": "#333",
	"b-c33": "#b93131", "b-ccc": "#3b3b3b", "b-ccc-desk": "#2a2a2a", "b-ccf": "#30305c", "b-ccf-l1": "#2c2c55",
	"b-ddd": "#4a4a4a", "b-e6e6f5": "#d0d0e0", "b-eee": "#2b2b2b",
	"b-eee-l0": "#2b2b2b", "b-f4f4ff": "#26263d", "b-f6f6f6": "#232323", "b-ff6": "#6a6400", "b-fff": "#1c1c1c",
	"b-fff-paper": "#fff", "b-ffffc9": "#3b381c", "b-rgba-0-0-0-03": "rgba(255,255,255,.035)",
	"b-rgba-0-0-0-12": "rgba(0,0,0,.45)", "b-rgba-0-0-0-15": "rgba(0,0,0,.5)",
	"f-1f7a3a": "#5fcf85", "f-262626": "#e2e2e2", "f-262626-p": "#e2e2e2",
	"f-339": "#8f8fff", "f-339-p": "#3d3dad", "f-66511a-p": "#66511a", "f-666-p": "#202020", "f-997a1f-p": "#997a1f", "f-999": "#6a6a6a", "f-999-p": "#5a5a5a",
	"f-c93": "#cc9933", "f-ca4-p": "#ccaa44", "f-ddd": "#3a3a3a", "f-fc3": "#ffcc33",
	"f-fff": "#1c1c1c", "f-fff-glint": "#fff", "f-fff-p": "#fff", "g-262626": "#a0a0a0", "g-353535cc": "#00000099",
	"g-777": "#131313", "g-9a9a9a": "#262626", "g-9c9c9ccc": "#3a3a3a99", "g-ccc": "#3b3b3b", "g-f1f1f1": "#6e6e6e",
	"g-fff": "#636363", "g-fff-doc": "#1c1c1c", "l-000": "#000", "l-000-ink": "#707070", "l-262626": "#000",
	"l-262626-ink": "#707070", "l-339": "#a9a9ff", "l-777": "#191919", "l-999": "#5c5c5c", "l-999-etch": "#262626", "l-9999fe": "#9999fe",
	"l-aaa": "#2c2c2c", "l-bbb": "#4c4c4c", "l-ccc": "#3b3b3b",
	"l-ddd": "#343434", "l-e4e4e4": "#2a2a2a", "l-eee": "#2a2a2a", "l-fff": "#6c6c6c",
	"l-fff-etch": "#646464", "s-000-p": "#000", "s-262626": "#000", "s-262626-ink": "#e2e2e2",
	"s-444-p": "#0c0c0c", "s-555-p": "#161616", "s-666-p": "#101010", "s-66c": "#33337a", "s-777": "#191919",
	"s-777-p": "#2e2e2e", "s-808080": "#191919", "s-888": "#141414", "s-888-p": "#3c3c3c", "s-911": "#5e0b0b",
	"s-911-p": "#5e0b0b", "s-999": "#191919", "s-999-p": "#323232", "s-aaa": "#2c2c2c", "s-aaa-p": "#404040",
	"s-bbb": "#2c2c2c", "s-ccf": "#8080e0", "s-ddd": "#4a4a4a", "s-e88": "#e08a8a", "s-e88-p": "#c06a6a", "s-fff": "#6c6c6c", "s-fff-p": "#6c6c6c",
	"s-rgba-0-0-0-25": "rgba(0,0,0,.55)", "s-rgba-255-255-255-50": "rgba(255,255,255,.1)",
	"s-rgba-255-255-255-60": "rgba(255,255,255,.14)", "s-rgba-38-38-38-85": "rgba(0,0,0,.9)", "t-000": "#e2e2e2",
	"t-1f7a3a": "#5fcf85", "t-262626": "#e2e2e2", "t-333": "#cdcdcd", "t-339": "#a9a9ff",
	"t-444": "#c4c4c4", "t-555": "#b4b4b4", "t-666": "#a2a2a2", "t-666-l0": "#8c8c8c", "t-6b6b6b": "#a2a2a2",
	"t-777": "#929292", "t-7a1010": "#ff9c94", "t-808080": "#8c8c8c", "t-999": "#7e7e7e",
	"t-aaa": "#686868", "t-b01c1c": "#ff8a80",
	"t-c33": "#ff8a80", "t-fff": "#fff", "t-fff-l34": "#fff", // the stats desk (exe-stats page.html): the chart's two lines keep
	// their hues, brighter on the dark paper, and what stands pale for a
	// day not yet over stands dim
	"b-ebebff": "#2a2a48", "b-f60": "#ff7a1a", "b-fc9": "#70401c", "b-ffc": "#3b381c", "f-99f": "#4a4a8a", "f-f60": "#ff7a1a",
	"f-fc9": "#70401c", "l-ccf": "#4a4a8a", "l-f60": "#ff7a1a", "s-444": "#0c0c0c", "s-555": "#161616",
	"s-rgba-0-0-0-35": "rgba(0,0,0,.6)",
	// the Appearance menu's moon keeps its pale face
	"f-ccf": "#ccccff",
}

// themeRoot is the chrome's own handful of variables (exe-stats
// chrome.css, :root), read here as the colours they are: a rule that
// says var(--g400) gets the night of #ccc in the job it does there.
var themeRoot = map[string]string{"black": "#262626", "g200": "#eee", "g300": "#ddd", "g400": "#ccc", "g500": "#bbb",
	"g600": "#999", "g700": "#808080", "g800": "#666", "hl": "#333399", "lav": "#ccf"}

// themeAlso is what the pages' scripts say in a rule of their own
// (hubMine's pressed reaction): defined with the rest, whether or not
// a rule of the stylesheet uses it.
var themeAlso = map[string]string{"l-339": "#333399", "t-339": "#333399"}

var (
	themeColour   = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\([^)]*\)|\b(?:white|black)\b|var\(--(?:black|g[2-8]00|hl|lav)(?:\s*,\s*[^)]*)?\)`)
	themeVarRef   = regexp.MustCompile(`^var\(--(\w+)`)
	themePicture  = regexp.MustCompile(`url\((?:'[^']*'|"[^"]*"|[^)]*)\)|(?:repeating-)?(?:linear|radial|conic)-gradient\(`)
	themeNest     = regexp.MustCompile(`^@(media|supports|layer|container)\b`)
	themeOnPaper  = regexp.MustCompile(`^(\.(av|post|embeds|text|card|cal|wk|rxs|quote|tk-plot|tk-flags)\b|img\b)`)
	themeHeld     = regexp.MustCompile(`:active|\[aria-(pressed|current|expanded)=true\]|\.on\b`)
	themeHeat     = regexp.MustCompile(`\.(cal|wk|archkey|act-leg)\b`)
	themeHeatStep = regexp.MustCompile(`\.l([0-4])\b`)
	themePaperBox = regexp.MustCompile(`img\.draw|\.wtile\b|\.dp-pad\b|\.pageview iframe`)
	themeGold     = regexp.MustCompile(`\.(bell|spark|sgl)\b`)
	themeEtch     = regexp.MustCompile(`(\.sep|-sep)\b`)
	themeDesk     = regexp.MustCompile(`^(html|body)\b`)
)

// themer reads stylesheets and keeps what it found in them.
type themer struct {
	day     map[string]string // variable -> the literal it stands for
	lost    map[string]string // a variable with no night -> a rule that uses it
	tokens  []string          // every colour replaced, in order, as it was written
	glyphs  int
	strange []string // a colour in a property no kind is known for
}

func newThemer() *themer {
	t := &themer{day: map[string]string{}, lost: map[string]string{}}
	for k, v := range themeAlso {
		t.day[k] = v
	}
	return t
}

// themeName is a colour as a variable's name says it: fff for white and
// #ffffff alike, 339 for #333399, rgba-0-0-0-12 for 12% black.
func themeName(lit string) string {
	s := strings.ToLower(strings.TrimSpace(lit))
	switch {
	case s == "white":
		return "fff"
	case s == "black":
		return "000"
	case strings.HasPrefix(s, "#"):
		s = s[1:]
		if len(s) == 6 && s[0] == s[1] && s[2] == s[3] && s[4] == s[5] {
			return string([]byte{s[0], s[2], s[4]})
		}
		return s
	}
	in := s[strings.Index(s, "(")+1 : strings.LastIndex(s, ")")]
	p := strings.Split(in, ",")
	for i := range p {
		p[i] = strings.TrimSpace(p[i])
	}
	a := "100"
	if len(p) > 3 {
		var f float64
		fmt.Sscanf("0"+strings.TrimPrefix(p[3], "0"), "%g", &f)
		a = fmt.Sprintf("%02d", int(f*100+0.5))
	}
	return "rgba-" + strings.Join(p[:3], "-") + "-" + a
}

func themeKind(prop string) string {
	switch {
	case prop == "color", prop == "-webkit-text-fill-color", prop == "text-decoration-color", prop == "caret-color", prop == "accent-color":
		return "t"
	case prop == "fill", prop == "stroke":
		return "f"
	case strings.HasPrefix(prop, "background"):
		return "b"
	case strings.HasPrefix(prop, "border"), strings.HasPrefix(prop, "outline"), strings.HasPrefix(prop, "column-rule"):
		return "l"
	case prop == "box-shadow", prop == "text-shadow":
		return "s"
	}
	return ""
}

// themeWhere is what a rule's selector says about where its colour
// stands, as the end of the variable's name; "" for nowhere particular.
func themeWhere(sel, kind, name, value string) string {
	s := strings.TrimSpace(sel)
	// kept white whatever the scheme: a drawing's paper, a page window's
	// page — what is drawn on them brought its own colours
	if kind == "b" && name == "fff" && themePaperBox.MatchString(s) {
		return "paper"
	}
	// the glint on the gold of the bell, the spark and the sun stays white
	if kind == "f" && name == "fff" && (themeGold.MatchString(s) || strings.Contains(s, ".sci .g")) && !strings.Contains(s, "aria-pressed") {
		return "glint"
	}
	// a menu's etched line: shadow over light
	if kind == "l" && themeEtch.MatchString(s) && (name == "999" || name == "fff") {
		return "etch"
	}
	// the heat scale of Archives and a profile's year counts up towards
	// the light by night, as it counts up towards the dark by day
	if themeHeat.MatchString(s) {
		m := themeHeatStep.FindStringSubmatch(s)
		if kind == "b" && m != nil {
			return "l" + m[1]
		}
		if kind == "t" && name == "fff" && strings.Contains(s, ".l3") {
			return "l34"
		}
		if kind == "t" && name == "666" && strings.Contains(s, ".l0") {
			return "l0"
		}
	}
	// the desk under the windows is a step under their grey by night
	if kind == "b" && name == "ccc" && themeDesk.MatchString(s) && !strings.Contains(s, ":has") {
		return "desk"
	}
	if kind == "g" && name == "fff" && strings.Contains(s, ".page::before") {
		return "doc"
	}
	// a black line drawn on the white frame — round a picture, a table,
	// a card: by night a grey one, or it is lost on the dark paper
	if themeOnPaper.MatchString(s) {
		if kind == "l" && (name == "262626" || name == "000") {
			return "ink"
		}
		if kind == "s" && name == "262626" && strings.Contains(value, "inset") {
			return "ink"
		}
	}
	// a button held down, a segment chosen: its bevel is lit from below
	held := themeHeld.MatchString(s) && !strings.Contains(s, ":disabled:active") && !strings.Contains(s, ".rxs .rx:active")
	if held && (kind == "s" || kind == "f" || (kind == "b" && (name == "666" || name == "888" || name == "262626"))) {
		return "p"
	}
	return ""
}

// css is one stylesheet with a variable where each colour stood.
func (t *themer) css(src string) string {
	var out strings.Builder
	i, n := 0, len(src)
	// scan reads to one of stop, outside strings, comments and brackets
	scan := func(stop string) string {
		j, depth := i, 0
		for ; j < n; j++ {
			c := src[j]
			if c == '/' && j+1 < n && src[j+1] == '*' {
				e := strings.Index(src[j+2:], "*/")
				if e < 0 {
					j = n
					break
				}
				j += e + 3
				continue
			}
			if c == '"' || c == '\'' {
				e := strings.IndexByte(src[j+1:], c)
				if e < 0 {
					j = n
					break
				}
				j += e + 1
				continue
			}
			if c == '(' {
				depth++
			} else if c == ')' {
				depth--
			} else if depth == 0 && strings.IndexByte(stop, c) >= 0 {
				break
			}
		}
		if j > n {
			j = n
		}
		text := src[i:j]
		i = j
		return text
	}
	colour := func(tok, kind, sel, prop, value string) string {
		lit := tok
		if m := themeVarRef.FindStringSubmatch(tok); m != nil {
			lit = themeRoot[m[1]]
		}
		name := themeName(lit)
		key := kind + "-" + name
		if w := themeWhere(sel, kind, name, value); w != "" {
			key += "-" + w
		}
		if _, ok := themeNight[key]; !ok {
			if _, seen := t.lost[key]; !seen {
				t.lost[key] = strings.Join(strings.Fields(sel), " ") + " { " + prop + " }"
			}
		}
		if _, ok := t.day[key]; !ok {
			t.day[key] = lit
		}
		t.tokens = append(t.tokens, tok)
		return "var(--" + key + ")"
	}
	decl := func(text, sel string) string {
		c := strings.IndexByte(text, ':')
		if c < 0 {
			return text
		}
		prop := strings.ToLower(strings.TrimSpace(text[:c]))
		if strings.HasPrefix(prop, "--") {
			return text
		}
		// a comment before the property is not its name
		if k := strings.LastIndex(prop, "*/"); k >= 0 {
			prop = strings.TrimSpace(prop[k+2:])
		}
		kind, value := themeKind(prop), text[c+1:]
		if kind == "" {
			if themeColour.MatchString(themePicture.ReplaceAllString(value, "")) && !strings.Contains(prop, "transition") {
				t.strange = append(t.strange, strings.Join(strings.Fields(sel), " ")+" { "+prop+" }")
			}
			return text
		}
		piece := func(s, kd string) string {
			return themeColour.ReplaceAllStringFunc(s, func(tok string) string { return colour(tok, kd, sel, prop, value) })
		}
		var res strings.Builder
		k := 0
		for k < len(value) {
			m := themePicture.FindStringIndex(value[k:])
			if m == nil {
				break
			}
			a, b := k+m[0], k+m[1]
			res.WriteString(piece(value[k:a], kind))
			if strings.HasPrefix(value[a:b], "url(") {
				u := value[a:b]
				if strings.Contains(u, `fill="black"`) {
					// a glyph drawn in black inside a data: picture: a variable
					// of its own, the glyph inked for the night
					key := ""
					for q, v := range t.day {
						if strings.HasPrefix(q, "i-") && v == u {
							key = q
						}
					}
					if key == "" {
						t.glyphs++
						key = fmt.Sprintf("i-%d", t.glyphs)
						t.day[key] = u
					}
					t.tokens = append(t.tokens, u)
					res.WriteString("var(--" + key + ")")
				} else {
					res.WriteString(u)
				}
				k = b
				continue
			}
			d, j := 1, b
			for ; j < len(value) && d > 0; j++ {
				if value[j] == '(' {
					d++
				} else if value[j] == ')' {
					d--
				}
			}
			res.WriteString(value[a:b])
			res.WriteString(piece(value[b:j], "g"))
			k = j
		}
		res.WriteString(piece(value[k:], kind))
		return text[:c+1] + res.String()
	}
	var rules func(top bool)
	rules = func(top bool) {
		for {
			pre := scan("{};")
			if i >= n {
				out.WriteString(pre)
				return
			}
			c := src[i]
			i++
			if c == '}' {
				out.WriteString(pre + "}")
				if !top {
					return
				}
				continue
			}
			if c == ';' {
				out.WriteString(pre + ";")
				continue
			}
			head := strings.TrimSpace(pre)
			if k := strings.LastIndex(head, "*/"); k >= 0 {
				head = strings.TrimSpace(head[k+2:])
			}
			if themeNest.MatchString(head) {
				out.WriteString(pre + "{")
				rules(false)
				continue
			}
			if strings.HasPrefix(head, "@") { // @keyframes, @font-face: as they are
				d, j := 1, i
				for ; j < n && d > 0; j++ {
					if src[j] == '{' {
						d++
					} else if src[j] == '}' {
						d--
					}
				}
				out.WriteString(pre + "{" + src[i:j])
				i = j
				continue
			}
			out.WriteString(pre + "{")
			// the paper look has a night of its own (web.html, .paper)
			skip := strings.Contains(head, ".paper") || head == ":root"
			for {
				d := scan(";}")
				if skip {
					out.WriteString(d)
				} else {
					out.WriteString(decl(d, head))
				}
				if i >= n {
					return
				}
				e := src[i]
				i++
				out.WriteByte(e)
				if e == '}' {
					break
				}
			}
		}
	}
	rules(true)
	return out.String()
}

// night is a variable's value on a dark page: the table's, a glyph
// picture inked, or, for a colour nobody gave a night, its day value.
func (t *themer) night(key string) string {
	if v, ok := themeNight[key]; ok {
		return v
	}
	if strings.HasPrefix(key, "i-") {
		return strings.ReplaceAll(t.day[key], `fill="black"`, `fill="%23`+strings.TrimPrefix(themeNight["t-262626"], "#")+`"`)
	}
	return t.day[key]
}

// scheme is the head of the stylesheet: every variable by day, and by
// night twice — for a dark system unless the reader chose Light, and
// for a reader who chose Dark. The replies frame (.framed) stays as the
// day draws it whatever the system: the page that frames it decides.
func (t *themer) scheme() string {
	keys := make([]string, 0, len(t.day))
	for k := range t.day {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var day, night strings.Builder
	for _, k := range keys {
		day.WriteString(" --" + k + ": " + t.day[k] + ";")
		night.WriteString(" --" + k + ": " + t.night(k) + ";")
	}
	return ":root { color-scheme: light;" + day.String() + " }\n" +
		"@media (prefers-color-scheme: dark) { :root:not([data-scheme=\"light\"]):not(.framed) { color-scheme: dark;" + night.String() + " } }\n" +
		":root[data-scheme=\"dark\"] { color-scheme: dark;" + night.String() + " }\n"
}

// The pages' stylesheets, read once: the template with its own sheet
// themed and the variables' block called for at its head, the stats
// desk's template with its sheet themed, and the chrome and the pop-up
// the template's {{chrome}} and {{popup}} now give.
var webTheme = func() (w struct {
	page, stats, chrome, popup, scheme string
	t                                  *themer
}) {
	t := newThemer()
	w.t = t
	w.chrome, w.popup = t.css(stats.ChromeCSS()), t.css(stats.PopupCSS())
	sheet := func(src, open string, hold map[string]string) string {
		a := strings.Index(src, open)
		if a < 0 {
			return src
		}
		a += len(open)
		b := a + strings.Index(src[a:], "</style>")
		in := src[a:b]
		for act, mark := range hold {
			in = strings.ReplaceAll(in, act, mark)
		}
		in = t.css(in)
		for act, mark := range hold {
			in = strings.ReplaceAll(in, mark, act)
		}
		return src[:a] + in + src[b:]
	}
	// the two actions inside the template's sheet stand as comments while it is read
	w.page = sheet(webHTML, "\n<style>\n", map[string]string{"{{chrome}}": "/*@chrome@*/", "{{popup}}": "/*@popup@*/", "{{schemevars}}": "/*@schemevars@*/"})
	w.stats = sheet(stats.TemplateHTML(), `{{define "statscss"}}<style>`, nil)
	w.scheme = t.scheme()
	return w
}()
