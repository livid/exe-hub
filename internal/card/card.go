// Package card unfurls the first link of a post into a link card: the
// page's OpenGraph title, a line of description and a small picture,
// fetched once at ingest and stored beside the post — outside the signed
// envelope, so signatures stay valid and every hub derives its own cards,
// replicated posts included (see PLAN.md, Link cards).
//
// The fetch runs under the same discipline as the peer embed mirror:
// hard timeouts, size-capped readers, and — because the link is untrusted
// text from a post — a dialer that refuses every non-public address, so a
// post can never point the hub at its own network.
package card

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// URL is the one URL matcher, shared with the public pages' linkifier:
// http(s) only; the period, comma or bracket that ends a sentence stays
// outside the link; and a URL is ASCII plus accented Latin letters, so
// CJK prose and its fullwidth punctuation end it — Chinese sits flush
// against a link with no space ("来自小虾平台（https://xiaoxia.app）的助手",
// "详见https://x.y的说明"). A raw CJK path loses its tail; browsers paste
// those percent-encoded.
const urlLatin = `\x{C0}-\x{D6}\x{D8}-\x{F6}\x{F8}-\x{24F}`

const urlPattern = `https?://[\w#$%&+,\-./:;=?@\[\]~` + urlLatin + `]*[\w#$%&+\-/;=@` + urlLatin + `]`

var URL = regexp.MustCompile(urlPattern)

// Link is a Markdown link, [words](url): the words, on one line and
// without brackets of their own, become the link. The address between
// the parentheses must be, whole, a URL the matcher above takes — so it
// is http(s) and nothing else, First finds the same address in the same
// text, and a link written this way unfurls into the same card as a
// bare one. Anything looser stays the text it was, its URL still linked.
// Submatch 1 is the words, 2 the address.
var Link = regexp.MustCompile(`\[([^\[\]\n]+)\]\((` + urlPattern + `)\)`)

// Unlink is text with every Markdown link put back to its words, for
// the places that show a post's words plain: an excerpt, a page title,
// a preview picture, a notification.
func Unlink(text string) string {
	return Link.ReplaceAllString(text, "$1")
}

// Code is an inline `code` span: no newlines, no nesting. It binds
// tighter than a link or bold, as in Markdown.
var Code = regexp.MustCompile("`([^`\n]+)`")

// Bold is Markdown's strong emphasis, **words**: two asterisks hard
// against the first and the last of the words, on one line, the words
// holding no asterisk of their own — so there is no nesting, and the
// asterisks of ordinary writing ("2 ** 3", "** note **", a lone "**")
// stay the characters they were. Submatch 1 is the words.
var Bold = regexp.MustCompile(`\*\*([^\s*](?:[^*\n]*[^\s*])?)\*\*`)

// Bolds are the bold stretches of a run of words, as Bold's submatch
// indexes, less those a claimed span cuts into: a code span (codes, as
// Code's match indexes) or a Markdown link (links, as Link's) that
// overlaps the stretch without sitting wholly inside its words leaves
// the asterisks literal. `**kwargs**` in backticks stays code, a link
// keeps its words whole — and **[words](url)** is a bold link, since
// the link lies inside. Bold within a link's words is the link's own
// to set: pass its words with no links.
func Bolds(text string, codes, links [][]int) [][]int {
	var out [][]int
	for _, m := range Bold.FindAllStringSubmatchIndex(text, -1) {
		free := true
		for _, c := range append(append([][]int{}, codes...), links...) {
			if c[0] < m[1] && c[1] > m[0] && !(c[0] >= m[2] && c[1] <= m[3]) {
				free = false
				break
			}
		}
		if free {
			out = append(out, m)
		}
	}
	return out
}

// Unbold is text with every bold stretch put back to its words, for the
// places that show a post's words plain — after Unlink, which has put
// the links back to theirs. Asterisks inside a code span stay.
func Unbold(text string) string {
	ms := Bolds(text, Code.FindAllStringIndex(text, -1), nil)
	if len(ms) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range ms {
		b.WriteString(text[last:m[0]])
		b.WriteString(text[m[2]:m[3]])
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

// First is the link a post's card is derived from: the first URL in its
// text, exactly as the linkifier would wrap it — skipping IPFS links,
// which are the linked pictures' business (a gateway serves a file, not
// a page with a title).
func First(text string) string {
	for _, u := range URL.FindAllString(text, -1) {
		if IPFSCID(u) == "" {
			return u
		}
	}
	return ""
}

// ---- post links: a link to a post here (PLAN.md, Post cards) ----

// postPath is a hub thread page's path, /p/ and the start of an id —
// eight hex characters or more, the whole id at most — with nothing
// after it but the query or fragment; the same shape the pages take.
var postPath = regexp.MustCompile(`^/p/([0-9a-fA-F]{8,64})/?$`)

// PostLink is the id, or the start of one, that a link to a hub post
// names, lower-case, or "" for any other link. Only the path says so —
// a hub does not know the names it is reached by, and a post pulled
// from a peer links to that peer's pages — so the caller asks the store
// whether a post here begins that way; one that does is the post the
// link means, and its card quotes it instead of fetching the page.
func PostLink(link string) string {
	pu, err := url.Parse(link)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
		return ""
	}
	if m := postPath.FindStringSubmatch(pu.Path); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// ---- IPFS links: the pictures a post links to (PLAN.md, Linked pictures) ----

// MaxPictures is how many linked pictures a post gets, the embed cap.
const MaxPictures = 4

// cidRE matches a CID in the shapes gateways serve: CIDv0 (Qm and 44
// base58btc characters) and CIDv1 in base32 (b…, what subdomain
// gateways use), base36 (k…) or base58btc (z…). A raw multihash or an
// inline identity CID is too short to be a picture and is not matched.
var cidRE = regexp.MustCompile(`^(?:Qm[1-9A-HJ-NP-Za-km-z]{44}|b[a-z2-7]{58,}|k[a-z0-9]{50,}|z[1-9A-HJ-NP-Za-km-z]{48,})$`)

// IPFSCID is the CID an IPFS link names, or "" for any other link: a URL
// whose path begins /ipfs/<cid> (a path gateway such as ipfs.io or
// ipfs.filebase.io) or whose host is <cid>.ipfs.<gateway> (a subdomain
// gateway such as dweb.link). What follows the CID — a path inside a
// directory, a query, a fragment — is the gateway's to resolve.
func IPFSCID(link string) string {
	pu, err := url.Parse(link)
	if err != nil {
		return ""
	}
	if rest, ok := strings.CutPrefix(pu.Path, "/ipfs/"); ok {
		c, _, _ := strings.Cut(rest, "/")
		if cidRE.MatchString(c) {
			return c
		}
		return ""
	}
	if c, rest, ok := strings.Cut(strings.ToLower(pu.Hostname()), "."); ok &&
		strings.HasPrefix(rest, "ipfs.") && cidRE.MatchString(c) {
		return c
	}
	return ""
}

// IPFSLinks lists the IPFS links in a post's text: in order, each once,
// at most MaxPictures of them, each exactly as the linkifier wraps it.
func IPFSLinks(text string) []string {
	var out []string
	for _, u := range URL.FindAllString(text, -1) {
		if IPFSCID(u) == "" || slices.Contains(out, u) {
			continue
		}
		out = append(out, u)
		if len(out) == MaxPictures {
			break
		}
	}
	return out
}

// Caps, PLAN.md: the page read stops at 1MB (OpenGraph lives in <head>),
// the picture at the embed cap of 8MB.
const (
	maxHTML  = 1 << 20
	MaxImage = 8 << 20
)

// Meta is what one page yielded. Title is never empty on success (the
// host stands in when the page names nothing); ImageURL is resolved
// absolute, or "".
type Meta struct {
	URL      string // the link as posted — what the card links back to
	Host     string // display host, "www." dropped
	Title    string
	Desc     string
	ImageURL string
}

// Fetcher fetches pages and pictures with the guarded client.
type Fetcher struct {
	client *http.Client
	// AllowPrivate lifts the public-address guard — tests only, where the
	// page under test lives on 127.0.0.1.
	AllowPrivate bool
}

func NewFetcher() *Fetcher {
	f := &Fetcher{}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	f.client = &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			// The guard runs on every dial, so a redirect to a private
			// address is refused the same as a direct link to one.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if f.AllowPrivate || publicIP(ip.IP) {
						return dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
					}
				}
				return nil, fmt.Errorf("%s resolves to no public address", host)
			},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
	return f
}

// publicIP is the address guard: loopback, RFC 1918, link-local, ULA,
// multicast, unspecified and the CGNAT range (100.64/10 — where Tailscale
// lives) are all refused.
func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 100 && ip4[1]&0xC0 == 64 {
			return false
		}
	} else if !ip.IsGlobalUnicast() {
		return false
	}
	return true
}

func (f *Fetcher) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "exe-hub/1 (link cards, linked pictures)")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("fetch %s: %s", u, resp.Status)
	}
	return resp, nil
}

// Fetch reads one page and returns its card metadata.
func (f *Fetcher) Fetch(ctx context.Context, link string) (*Meta, error) {
	pu, err := url.Parse(link)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Hostname() == "" {
		return nil, fmt.Errorf("not a fetchable link: %q", link)
	}
	resp, err := f.get(ctx, link)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" &&
		!strings.Contains(ct, "text/html") && !strings.Contains(ct, "application/xhtml") {
		return nil, fmt.Errorf("not a page: %s", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTML))
	if err != nil {
		return nil, err
	}
	m := parseMeta(decode(body, resp.Header.Get("Content-Type")))
	m.URL = link
	m.Host = strings.TrimPrefix(pu.Hostname(), "www.")
	if m.Title == "" {
		m.Title = m.Host
	}
	// og:image may be relative; resolve against the page as served
	// (redirects included), http(s) only.
	if m.ImageURL != "" {
		iu, err := resp.Request.URL.Parse(m.ImageURL)
		if err != nil || (iu.Scheme != "http" && iu.Scheme != "https") {
			m.ImageURL = ""
		} else {
			m.ImageURL = iu.String()
		}
	}
	return m, nil
}

// ErrNotPicture is a link that answered with something other than a
// picture, or with one past the cap: what it names will never be one.
var ErrNotPicture = errors.New("not a picture")

// FetchImage reads a picture — a card's, or one a post links to on IPFS
// — capped at the embed size, and sniffs the real type: only what
// sniffs as an image is accepted (SVG never does, which keeps
// scriptable content out of /v1/embed). The answer that is not a
// picture is ErrNotPicture; any other error is the line or the server.
func (f *Fetcher) FetchImage(ctx context.Context, u string) (data []byte, mime string, err error) {
	if pu, err := url.Parse(u); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Hostname() == "" {
		return nil, "", fmt.Errorf("not a fetchable link: %q", u)
	}
	resp, err := f.get(ctx, u)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, MaxImage+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > MaxImage {
		return nil, "", fmt.Errorf("%w: past the %dMB cap", ErrNotPicture, MaxImage>>20)
	}
	mime = http.DetectContentType(data)
	if i := strings.Index(mime, ";"); i > 0 {
		mime = mime[:i]
	}
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", fmt.Errorf("%w: %s", ErrNotPicture, mime)
	}
	return data, mime, nil
}

// ---- the page's charset ----

var charsetParam = regexp.MustCompile(`(?i)charset\s*=\s*["']?\s*([a-z0-9_:.\-]+)`)

// decode turns the page into UTF-8 the way a browser finds its charset:
// the Content-Type header's parameter, else the page's own <meta charset>
// or http-equiv declaration; a page naming none is read as UTF-8. Much of
// the Japanese and Chinese web still serves Shift_JIS, EUC-JP or GBK with
// a bare "text/html" header, and read as UTF-8 their titles came out as
// raw bytes. Bytes that still are not UTF-8 after this (an unknown label,
// an undeclared legacy page) are dropped, so a card never stores them.
func decode(body []byte, contentType string) string {
	label := ""
	if m := charsetParam.FindStringSubmatch(contentType); m != nil {
		label = m[1]
	} else {
		// Every legacy charset a page can declare keeps ASCII where it is,
		// so the declaration is readable before the page is decoded.
		for _, tag := range metaTag.FindAll(body, -1) {
			if m := charsetParam.FindSubmatch(tag); m != nil {
				label = string(m[1])
				break
			}
		}
	}
	if label != "" {
		if enc, err := htmlindex.Get(label); err == nil {
			if out, err := enc.NewDecoder().Bytes(body); err == nil {
				body = out
			}
		}
	}
	return strings.ToValidUTF8(string(body), "")
}

// Misread says a stored card's text is not UTF-8 — a card derived before
// decode existed, from a page in a legacy charset. Backfill derives those
// again; a card from decode is always UTF-8, so none is redone twice.
func Misread(title, desc string) bool {
	return !utf8.ValidString(title) || !utf8.ValidString(desc)
}

// ---- the metadata scan ----

var (
	metaTag  = regexp.MustCompile(`(?is)<meta\s[^>]*>`)
	attrPair = regexp.MustCompile(`(?is)([a-z][a-z0-9:_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	titleTag = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// parseMeta scans the page's meta tags without a DOM: attributes come in
// any order and either quoting. Priority is OpenGraph, then Twitter's
// names, then the plain title and description.
func parseMeta(page string) *Meta {
	vals := map[string]string{}
	for _, tag := range metaTag.FindAllString(page, -1) {
		var key, content string
		for _, a := range attrPair.FindAllStringSubmatch(tag, -1) {
			v := a[2] + a[3] + a[4]
			switch strings.ToLower(a[1]) {
			case "property", "name":
				key = strings.ToLower(v)
			case "content":
				content = v
			}
		}
		if key != "" && content != "" {
			if _, seen := vals[key]; !seen { // the first of a kind wins
				vals[key] = content
			}
		}
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := clean(vals[k]); v != "" {
				return v
			}
		}
		return ""
	}
	m := &Meta{
		Title:    pick("og:title", "twitter:title"),
		Desc:     pick("og:description", "twitter:description", "description"),
		ImageURL: pick("og:image", "og:image:url", "twitter:image"),
	}
	if m.Title == "" {
		if t := titleTag.FindStringSubmatch(page); t != nil {
			m.Title = clean(t[1])
		}
	}
	m.Title = cut(m.Title, 200)
	m.Desc = cut(m.Desc, 300)
	return m
}

// clean unescapes entities and collapses whitespace to single spaces.
func clean(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

// cut caps a string at n runes, ending a cut one with an ellipsis.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}
