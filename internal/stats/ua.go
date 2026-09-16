package stats

import "strings"

// Classify reads a user agent into what the stats keep of it: the
// device (desktop, mobile, tablet, or agent for a tool), the browser
// and the operating system — names, never the string itself — and
// whether it is a crawler, whose visits are not counted at all. Tools
// are kept apart from crawlers: an agent reading skill.md with curl or
// fetch is a reader this hub wants to count; a search engine's crawler,
// a link unfurler or a headless test browser is not.
func Classify(ua string) (device, browser, os string, bot bool) {
	l := strings.ToLower(ua)
	if l == "" {
		return "agent", "", "", false
	}
	for _, b := range bots {
		if strings.Contains(l, b) {
			return "", "", "", true
		}
	}
	for _, t := range tools {
		if strings.Contains(l, t.mark) {
			return "agent", t.name, "", false
		}
	}
	has := func(s string) bool { return strings.Contains(l, s) }
	switch {
	case has("iphone"), has("ipod"):
		os = "iOS"
		device = "mobile"
	case has("ipad"):
		os = "iOS"
		device = "tablet"
	case has("harmonyos"):
		os = "HarmonyOS"
		device = mobileOrTablet(l)
	case has("android"):
		os = "Android"
		device = mobileOrTablet(l)
	case has("windows phone"):
		os = "Windows"
		device = "mobile"
	case has("windows"):
		os = "Windows"
		device = "desktop"
	case has("cros"):
		os = "ChromeOS"
		device = "desktop"
	case has("macintosh"), has("mac os x"):
		os = "macOS"
		device = "desktop"
	case has("freebsd"):
		os = "FreeBSD"
		device = "desktop"
	case has("linux"):
		os = "Linux"
		device = "desktop"
	default:
		device = "desktop"
	}
	if device == "desktop" && (has("mobile") || has("tablet")) {
		device = "mobile"
		if has("tablet") {
			device = "tablet"
		}
	}
	switch {
	case has("micromessenger"):
		browser = "WeChat"
	case has("edg/"), has("edga/"), has("edgios/"), has("edge/"):
		browser = "Edge"
	case has("opr/"), has("opera"):
		browser = "Opera"
	case has("samsungbrowser"):
		browser = "Samsung Internet"
	case has("ucbrowser"), has("ubrowser"):
		browser = "UC Browser"
	case has("qqbrowser"):
		browser = "QQ Browser"
	case has("miuibrowser"):
		browser = "Mi Browser"
	case has("huaweibrowser"):
		browser = "Huawei Browser"
	case has("quark"):
		browser = "Quark"
	case has("vivaldi"):
		browser = "Vivaldi"
	case has("yabrowser"):
		browser = "Yandex Browser"
	case has("duckduckgo"):
		browser = "DuckDuckGo"
	case has("brave"):
		browser = "Brave"
	case has("fxios"), has("firefox/"):
		browser = "Firefox"
	case has("crios"), has("chrome/"), has("chromium/"):
		browser = "Chrome"
	case has("msie"), has("trident/"):
		browser = "Internet Explorer"
	case has("safari/"), os == "iOS" && has("applewebkit"):
		browser = "Safari"
	default:
		browser = ""
	}
	return device, browser, os, false
}

func mobileOrTablet(l string) string {
	if strings.Contains(l, "mobile") {
		return "mobile"
	}
	return "tablet"
}

// bots are crawlers, unfurlers, monitors and headless browsers: not
// readers. Matched as substrings of the lower-cased user agent; "bot"
// alone catches Googlebot, bingbot, Twitterbot, GPTBot, ClaudeBot and
// the rest of that family.
var bots = []string{
	"bot", "crawl", "spider", "slurp", "archive.org", "ia_archiver", "facebookexternalhit",
	"whatsapp", "telegram", "discord", "embedly", "bingpreview", "skypeuripreview",
	"headlesschrome", "lighthouse", "pagespeed", "gtmetrix", "feedfetcher", "google-read-aloud",
	"phantomjs", "playwright", "puppeteer", "selenium", "scrapy", "wappalyzer", "dataprovider",
}

// tools are what agents and scripts read with; a tool's visit is a
// reader's, kept under device "agent" with the tool for its browser.
var tools = []struct{ mark, name string }{
	{"claude-user", "Claude"}, {"claude-code", "Claude"}, {"chatgpt-user", "ChatGPT"}, {"perplexity-user", "Perplexity"},
	{"curl/", "curl"}, {"wget", "Wget"}, {"go-http-client", "Go"}, {"python-requests", "Python"},
	{"python-urllib", "Python"}, {"aiohttp", "Python"}, {"httpx", "Python"}, {"python", "Python"},
	{"node-fetch", "Node"}, {"undici", "Node"}, {"axios", "Node"}, {"node", "Node"}, {"deno", "Deno"}, {"bun/", "Bun"},
	{"okhttp", "OkHttp"}, {"java/", "Java"}, {"libwww-perl", "Perl"}, {"httpie", "HTTPie"}, {"powershell", "PowerShell"},
	{"ruby", "Ruby"}, {"php", "PHP"}, {"postman", "Postman"}, {"insomnia", "Insomnia"}, {"guzzle", "PHP"},
	{"libcurl", "curl"}, {"dart", "Dart"}, {"reqwest", "Rust"}, {"hyper/", "Rust"},
}
