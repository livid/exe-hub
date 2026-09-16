package stats

import (
	"regexp"
	"strings"
)

// Classify reads a user agent into what the stats keep of it: the
// device (desktop, mobile, tablet, agent for a tool, or bot), the
// browser and the operating system — names, never the string itself —
// and whether it is a crawler. A crawler's hits are kept apart from
// people's: counted under device "bot" with the crawler's name for a
// browser (BotName), left out of every human number and shown in the
// Bots window. Tools are not crawlers: an agent reading skill.md with
// curl or fetch is a reader this hub wants to count as one.
func Classify(ua string) (device, browser, os string, bot bool) {
	l := strings.ToLower(ua)
	if l == "" {
		return "agent", "", "", false
	}
	for _, b := range bots {
		if strings.Contains(l, b) {
			return "bot", BotName(ua), "", true
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

// BotName names a crawler from its user agent: the well-known ones by
// name, else the token that says bot, crawler or spider (SemrushBot,
// MJ12bot), else the first product token. Never the string itself.
func BotName(ua string) string {
	l := strings.ToLower(ua)
	for _, b := range botNames {
		if strings.Contains(l, b.mark) {
			return b.name
		}
	}
	if m := botToken.FindString(ua); m != "" {
		return clip(m, 48)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(ua), "/")
	first, _, _ = strings.Cut(first, " ")
	if first == "" {
		return "Bot"
	}
	return clip(first, 48)
}

var botToken = regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:bot|crawler|spider)[A-Za-z0-9_.-]*`)

var botNames = []struct{ mark, name string }{
	{"googlebot", "Googlebot"}, {"google-inspectiontool", "Googlebot"}, {"storebot-google", "Googlebot"}, {"adsbot-google", "AdsBot"},
	{"bingbot", "Bingbot"}, {"bingpreview", "Bingbot"}, {"msnbot", "Bingbot"}, {"duckduckbot", "DuckDuckBot"}, {"duckassistbot", "DuckDuckBot"},
	{"yandexbot", "Yandex"}, {"yandex", "Yandex"}, {"baiduspider", "Baidu"}, {"sogou", "Sogou"}, {"360spider", "360"},
	{"applebot", "Applebot"}, {"amazonbot", "Amazonbot"}, {"petalbot", "PetalBot"}, {"bytespider", "Bytespider"}, {"tiktokspider", "Bytespider"},
	{"gptbot", "GPTBot"}, {"oai-searchbot", "OAI-SearchBot"}, {"chatgpt", "ChatGPT"}, {"claudebot", "ClaudeBot"}, {"anthropic", "ClaudeBot"},
	{"perplexitybot", "PerplexityBot"}, {"ccbot", "CCBot"}, {"meta-externalagent", "Meta"}, {"facebookexternalhit", "Facebook"},
	{"facebookbot", "Facebook"}, {"twitterbot", "X"}, {"linkedinbot", "LinkedIn"}, {"slackbot", "Slack"}, {"slack-imgproxy", "Slack"},
	{"telegrambot", "Telegram"}, {"discordbot", "Discord"}, {"whatsapp", "WhatsApp"}, {"pinterestbot", "Pinterest"}, {"redditbot", "Reddit"},
	{"mastodon", "Mastodon"}, {"bluesky", "Bluesky"}, {"archive.org_bot", "Internet Archive"}, {"ia_archiver", "Internet Archive"},
	{"semrushbot", "Semrush"}, {"ahrefsbot", "Ahrefs"}, {"mj12bot", "Majestic"}, {"dotbot", "Moz"}, {"dataforseobot", "DataForSEO"},
	{"uptimerobot", "UptimeRobot"}, {"pingdom", "Pingdom"}, {"headlesschrome", "HeadlessChrome"}, {"lighthouse", "Lighthouse"},
	{"playwright", "Playwright"}, {"puppeteer", "Puppeteer"}, {"scrapy", "Scrapy"}, {"wappalyzer", "Wappalyzer"}, {"embedly", "Embedly"},
	{"google-read-aloud", "Google Read Aloud"}, {"feedfetcher", "Feedfetcher"}, {"phantomjs", "PhantomJS"},
}
