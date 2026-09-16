package stats

import (
	"net/url"
	"strings"
)

// Source names where a visit came from, from the Referer header: the
// well-known sites by name (Google, X, Hacker News, V2EX…), any other
// site by its host, a link from this hub itself or no referrer at all
// as direct. The channel groups sources: search, social, ai (a chat
// assistant's link), referral, direct; a tagged link (utm_source) is
// its own channel, campaign, set by the caller.
func Source(ref, self string) (name, channel string) {
	if ref == "" {
		return "", "direct"
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return "", "direct"
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "android-app" {
		// android-app://com.google.android.googlequicksearchbox: a phone's search box
		if s, ok := apps[host]; ok {
			return s.name, s.channel
		}
		return host, "referral"
	}
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	if selfHost, _, err := splitHost(self); err == nil && host == strings.TrimPrefix(strings.ToLower(selfHost), "www.") {
		return "", "direct"
	}
	for h := host; h != ""; {
		if s, ok := sites[h]; ok {
			return s.name, s.channel
		}
		// google.co.uk, yandex.ru: the site by its first label
		i := strings.IndexByte(h, '.')
		if i < 0 {
			break
		}
		if s, ok := labels[h[:i]]; ok && strings.Count(h, ".") <= 2 {
			return s.name, s.channel
		}
		h = h[i+1:]
	}
	return clip(host, 128), "referral"
}

func splitHost(hostport string) (string, string, error) {
	if i := strings.LastIndexByte(hostport, ':'); i > 0 && !strings.Contains(hostport[i:], "]") {
		return hostport[:i], hostport[i+1:], nil
	}
	return hostport, "", nil
}

type site struct{ name, channel string }

var sites = map[string]site{
	"bing.com": {"Bing", "search"}, "duckduckgo.com": {"DuckDuckGo", "search"}, "baidu.com": {"Baidu", "search"},
	"ecosia.org": {"Ecosia", "search"}, "search.brave.com": {"Brave Search", "search"}, "sogou.com": {"Sogou", "search"},
	"so.com": {"360 Search", "search"}, "startpage.com": {"Startpage", "search"}, "qwant.com": {"Qwant", "search"},
	"kagi.com": {"Kagi", "search"}, "naver.com": {"Naver", "search"}, "seznam.cz": {"Seznam", "search"},
	"perplexity.ai": {"Perplexity", "ai"}, "chatgpt.com": {"ChatGPT", "ai"}, "chat.openai.com": {"ChatGPT", "ai"},
	"claude.ai": {"Claude", "ai"}, "gemini.google.com": {"Gemini", "ai"}, "copilot.microsoft.com": {"Copilot", "ai"},
	"t.co": {"X", "social"}, "twitter.com": {"X", "social"}, "x.com": {"X", "social"}, "mobile.twitter.com": {"X", "social"},
	"facebook.com": {"Facebook", "social"}, "fb.com": {"Facebook", "social"}, "l.facebook.com": {"Facebook", "social"},
	"lm.facebook.com": {"Facebook", "social"}, "instagram.com": {"Instagram", "social"}, "l.instagram.com": {"Instagram", "social"},
	"linkedin.com": {"LinkedIn", "social"}, "lnkd.in": {"LinkedIn", "social"}, "reddit.com": {"Reddit", "social"},
	"old.reddit.com": {"Reddit", "social"}, "out.reddit.com": {"Reddit", "social"}, "t.me": {"Telegram", "social"},
	"telegram.me": {"Telegram", "social"}, "web.telegram.org": {"Telegram", "social"}, "news.ycombinator.com": {"Hacker News", "social"},
	"threads.net": {"Threads", "social"}, "threads.com": {"Threads", "social"}, "bsky.app": {"Bluesky", "social"},
	"mastodon.social": {"Mastodon", "social"}, "youtube.com": {"YouTube", "social"}, "youtu.be": {"YouTube", "social"},
	"weibo.com": {"Weibo", "social"}, "weibo.cn": {"Weibo", "social"}, "zhihu.com": {"Zhihu", "social"},
	"v2ex.com": {"V2EX", "social"}, "discord.com": {"Discord", "social"}, "discordapp.com": {"Discord", "social"},
	"slack.com": {"Slack", "social"}, "whatsapp.com": {"WhatsApp", "social"}, "pinterest.com": {"Pinterest", "social"},
	"tiktok.com": {"TikTok", "social"}, "douyin.com": {"Douyin", "social"}, "bilibili.com": {"Bilibili", "social"},
	"xiaohongshu.com": {"Xiaohongshu", "social"}, "lobste.rs": {"Lobsters", "social"}, "producthunt.com": {"Product Hunt", "social"},
	"github.com": {"GitHub", "referral"}, "medium.com": {"Medium", "referral"}, "substack.com": {"Substack", "referral"},
	"mail.google.com": {"Gmail", "referral"}, "outlook.live.com": {"Outlook", "referral"}, "duckduckgo.com/": {"DuckDuckGo", "search"},
}

// labels are sites with many country domains, matched by the first
// label of a two- or three-label host: google.com, google.co.uk,
// yandex.ru, yahoo.co.jp.
var labels = map[string]site{
	"google": {"Google", "search"}, "yandex": {"Yandex", "search"}, "yahoo": {"Yahoo", "search"},
}

var apps = map[string]site{
	"com.google.android.googlequicksearchbox": {"Google", "search"},
	"com.google.android.gm":                   {"Gmail", "referral"},
	"org.telegram.messenger":                  {"Telegram", "social"},
	"com.twitter.android":                     {"X", "social"},
	"com.slack":                               {"Slack", "social"},
	"com.linkedin.android":                    {"LinkedIn", "social"},
	"com.reddit.frontpage":                    {"Reddit", "social"},
	"com.tencent.mm":                          {"WeChat", "social"},
}
