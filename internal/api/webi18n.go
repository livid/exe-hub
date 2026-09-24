package api

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	stats "github.com/livid/exe-stats"
)

// The pages' own words in the reader's language (PLAN.md, Public pages
// — The pages in the reader's language). Three languages: English,
// Simplified Chinese and Japanese; every word of chrome — the join
// window, the pager, the find strip, the Post window and its Profile
// dialog, the picture viewer, the error pages, the titles, the line
// under a translated post — comes from one table, webStrings, and a
// test holds the three columns to the same keys and the same
// placeholders. The posts stay in whatever language they were written
// (see Translations for what a reader is shown).
//
// The template is parsed once for each language with T bound to it, so
// {{T "key"}} stands anywhere in the page, in a nested template as in
// the top one, with nothing to carry; the scripts get the same table as
// JSON (webData.JS) and their own T. A string with a value in it says
// where with {name}; T "key" "name" value fills it, and a count named n
// of one picks "key.one" where the language has one (English does, the
// other two never inflect). The numbers the heartbeat rewrites in place
// (members, posts, online) stand outside their word, in the template,
// so those keys are the word alone and every language puts it after
// the number.

// webLocale is one language the pages speak.
type webLocale struct {
	Code string            // en, zh, ja: the table's key and ?lang='s short form
	Tag  string            // the <html lang>: en, zh-Hans, ja
	S    map[string]string // the words, by key
}

var webLocales = map[string]*webLocale{
	"en": {Code: "en", Tag: "en", S: webStrings["en"]},
	"zh": {Code: "zh", Tag: "zh-Hans", S: webStrings["zh"]},
	"ja": {Code: "ja", Tag: "ja", S: webStrings["ja"]},
}

// webLocaleOf is the language the page's own words are in: ?lang= when
// the request names one the pages speak — zh, ja or en, or a fuller tag
// of one (zh-TW reads the Simplified chrome; orig and fr fall through)
// — else the browser's first language, the Accept-Language tag with the
// highest q, else English. Decided on the server, so the page stands
// without script and never flashes from one language to another; every
// page says Vary: Accept-Language.
func webLocaleOf(r *http.Request) *webLocale {
	if l := webLocaleTag(strings.ToLower(r.URL.Query().Get("lang"))); l != nil {
		return l
	}
	if l := webLocaleTag(webBrowserLang(r)); l != nil {
		return l
	}
	return webLocales["en"]
}

// webLocaleTag is the locale a lower-cased tag names, nil for a language
// the pages do not speak.
func webLocaleTag(tag string) *webLocale {
	base, _, _ := strings.Cut(tag, "-")
	return webLocales[base]
}

// T is the string named key in this language, with each {name} filled
// from the name, value pairs that follow; a count named n of one picks
// the "key.one" form where there is one.
func (l *webLocale) T(key string, kv ...any) string {
	s, ok := l.S[key]
	if !ok {
		s = key // never, the test says; a key on the page beats a blank
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] == "n" && isOne(kv[i+1]) {
			if one, ok := l.S[key+".one"]; ok {
				s = one
			}
		}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		s = strings.ReplaceAll(s, "{"+fmt.Sprint(kv[i])+"}", fmt.Sprint(kv[i+1]))
	}
	return s
}

// JS is what the page's scripts get, as one JSON object in the head:
// the strings (s) and the locale the dates are set in (loc, "" for the
// browser's own).
func (d *webData) JS() map[string]any { return map[string]any{"s": d.L.S, "loc": d.DateLoc} }

func isOne(v any) bool {
	switch n := v.(type) {
	case int:
		return n == 1
	case int64:
		return n == 1
	case int32:
		return n == 1
	}
	return false
}

// webTmpls is the page template, parsed once for each language with T
// bound to it; the stats desk's blocks come from the package that draws
// it (its own words are still English), parsed into each set so the
// desk renders inside the hub's chrome.
var webTmpls = func() map[string]*template.Template {
	out := map[string]*template.Template{}
	for code, l := range webLocales {
		t := template.New("web").Funcs(stats.Funcs()).Funcs(template.FuncMap{"T": l.T})
		out[code] = template.Must(template.Must(t.Parse(webHTML)).Parse(stats.TemplateHTML()))
	}
	return out
}()

// webStrings is every word of chrome on the pages, in the three
// languages, by key. The English column is the reference and carries
// the comments; a string that takes a value says where with {name}. The
// Chinese and Japanese are written as their readers write, not glossed
// from the English, and keep the words the hub keeps in English — hub,
// agent, peer, mint, Connect… as the Hub app's menu says it — with the
// half-width space between Han or kana and Latin that the pages' dates
// have. OK, Cancel and Save are the words a Mac OS 9 of that language
// wears.
var webStrings = map[string]map[string]string{
	"en": {
		// the feed
		"nothing":      "Nothing here yet.",
		"find":         "Search posts", // the find strip's field and its label
		"search":       "Search",       // its button, the search window's title, the page's <title>
		"notify.off":   "Notify me of every new post",
		"notify.on":    "Notifying you of every new post",
		"language":     "Language", // the menu beside the bell, and on a thread's top strip
		"prev":         "Prev",
		"next":         "Next",
		"members":      "members", // after the number the heartbeat rewrites
		"posts":        "posts",   // the same, and a profile's count
		"online":       "online",  // the same
		"online.title": "Who reads this hub",
		"matches":      "{n} posts match",
		"matches.one":  "{n} post matches",
		"nomatch":      "No post matches.",
		"search.hint":  "Type a word or two, then Search. Every word must appear in the post.",
		// a thread
		"back":          "Back to the feed", // the title bar's box, and the stats desk's way home
		"feed":          "Feed",             // the button back to it
		"replies":       "{n} replies",      // the thread's status line and a root's foot
		"replies.one":   "{n} reply",
		"replies.range": "{a}–{b} of {n} replies", // a thread page past one page: the replies this page holds
		"summary":       "Summary",                // the window beside a long thread, and its meta line
		"summary.of":    "Summary of the first {n} replies",
		"summarize":     "Summarize", // the strip's button on a phone, which opens the window
		"reply":         "Reply",     // the link under a reply, the Reply window's title and button
		"inreply":       "in reply to",
		"inreply.name":  "in reply to {name}",
		// a profile
		"since.pre":  "since ", // around the day the profile was first seen; the day goes between
		"since.post": "",
		"noposts":    "No posts yet.",
		// a post's embeds
		"archived": "Archived copy",
		"picture":  "Picture", // a picture's name when the file had none
		"sound":    "Sound",
		"page":     "Page",
		"download": "download",
		// the picture and page viewer
		"close":     "Close",
		"zoom":      "Zoom",
		"loading":   "Loading…",
		"page.fail": "Could not load the page: {err}",
		// the button beside a fenced code block, and its word for a moment after a press
		"copy":   "Copy",
		"copied": "Copied",
		// the line under a translated post
		"tr.from": "Translated from {lang}",
		"tr.show": "Show Original",
		"tr.back": "Show Translation",
		// the Post window
		"post":               "Post",
		"compose.note.post":  "Post from a Solana wallet: one signature a post, never a transaction.",
		"compose.note.reply": "Reply from a Solana wallet: one signature a post, never a transaction.",
		"signin":             "Sign in with Solana",
		"yourpage":           "Your page",
		"profile":            "Profile…",
		"signout":            "Sign Out",
		"re.clear":           "Answer the thread’s post instead",
		"placeholder.post":   "What’s happening?",
		"placeholder.reply":  "Write a reply",
		"text.post":          "Post text", // the field's label for a screen reader
		"text.reply":         "Reply text",
		"mention":            "Mention",
		"checking":           "Checking this address…",
		"choose.wallet":      "Choose a wallet:",
		"cancel":             "Cancel",
		"nowallet":           "No Solana wallet in this browser.",
		"declined":           "Sign-in was declined in the wallet.",
		"noconnect":          "The wallet did not connect: {err}",
		"noaccount":          "no account", // the err in noconnect when the wallet gave none
		"banned":             "This key is banned from posting here.",
		"below.post":         "This address holds less than {need}, so it can read but not post.",
		"below.reply":        "This address holds less than {need}, so it can read but not reply.",
		"unavailable":        "The hub can’t check holdings right now. Try again later.",
		"gone":               "That reply is gone. Clear it to answer the thread instead.",
		"again.post":         "You can post again in {s} s.",
		"again.reply":        "You can reply again in {s} s.",
		"each.post":          "Each post asks your wallet for one signature.",
		"each.reply":         "Each reply asks your wallet for one signature.",
		"over":               "That is {n} bytes past the {max}-byte limit.",
		"check.fail":         "Could not check this address: {err}",
		"http":               "HTTP {code}",
		"units.raw":          "{n} raw units", // a holding, and what the gate asks
		"units.tokens":       "{n} tokens",
		"of.mint":            "{units} of {mint}", // the holding and the mint it is of
		"or":                 " or ",              // between two mints
		"changed":            "The wallet changed the message before signing it, so the hub could not verify it.",
		"youdeclined":        "You declined in the wallet.",
		"nosign":             "The wallet could not sign: {err}",
		"waiting":            "Waiting for your wallet…",
		"saving":             "Saving…",
		"sending":            "Sending…",
		"badsig":             "The hub could not verify the wallet’s signature.",
		"landed":             "Another message from this key landed first. Press {btn} again.",
		"replying.pre":       "Replying to ", // around the name of the reply being answered
		"replying.post":      "",
		"posted":             "Posted.",
		"replied":            "Replied.",
		// the Profile dialog
		"profile.title": "Profile",
		"pic":           "Picture",
		"choose":        "Choose Picture…",
		"name":          "Name",
		"display.name":  "Display name",
		"holding":       "Holding",
		"edit":          "Edit",
		"ok":            "OK",
		"save":          "Save",
		"noname":        "No name yet",
		"nogate":        "This hub has no token gate.",
		"unknown":       "Unknown: the hub can’t read holdings right now.",
		"admin":         "An admin key: the gate does not apply.",
		"enough":        "Enough to post. The hub asks for {need}.",
		"less":          "Less than the {need} the hub asks for, so this address can read but not post.",
		"pic.max":       "A picture is at most 8 MB.",
		"pic.store":     "The hub can’t store pictures right now. Try again later.",
		"pic.type":      "That file isn’t a picture the hub can read. Try a PNG, JPEG or GIF.",
		"uploading":     "Uploading…",
		"pic.save":      "Save to use this picture.",
		"name.req":      "A name is required.",
		"name.max":      "A name is at most {n} bytes.",
		"saved":         "Profile saved.",
		// titles
		"title.on": "{name} on {host}", // a profile's page, and a thread whose post has no words
		"stats":    "Stats",
		// the error page
		"error.back":    "Back to the feed.",
		"err.nopage":    "No such page.",
		"err.feed":      "The feed could not be read.",
		"err.search":    "The posts could not be searched.",
		"err.ambiguous": "More than one post begins that way. Use the whole id.",
		"err.nopost":    "No such post.",
		"err.post":      "The post could not be read.",
		"err.thread":    "The thread could not be read.",
		"err.posts":     "The posts could not be read.",
		"err.noprofile": "No such profile.",
		"err.profile":   "The profile could not be read.",
		"err.nostats":   "No stats on this hub.",
		"err.stats":     "The stats could not be read.",
	},
	"zh": {
		"nothing":            "这里还没有内容。",
		"find":               "搜索帖子",
		"search":             "搜索",
		"notify.off":         "有新帖时通知我",
		"notify.on":          "已订阅新帖通知",
		"language":           "语言",
		"prev":               "上一页",
		"next":               "下一页",
		"members":            "位成员",
		"posts":              "条帖子",
		"online":             "人在线",
		"online.title":       "谁在读这个 hub",
		"matches":            "{n} 条帖子匹配",
		"nomatch":            "没有匹配的帖子。",
		"search.hint":        "输入一两个词，然后搜索。每个词都必须出现在帖子里。",
		"back":               "返回信息流",
		"feed":               "信息流",
		"replies":            "{n} 条回复",
		"replies.range":      "第 {a}–{b} 条，共 {n} 条回复",
		"summary":            "摘要",
		"summary.of":         "前 {n} 条回复的摘要",
		"summarize":          "总结",
		"reply":              "回复",
		"inreply":            "回复的帖子",
		"inreply.name":       "回复 {name}",
		"since.pre":          "加入于 ",
		"since.post":         "",
		"noposts":            "还没有帖子。",
		"archived":           "存档副本",
		"picture":            "图片",
		"sound":              "声音",
		"page":               "页面",
		"download":           "下载",
		"close":              "关闭",
		"zoom":               "缩放",
		"loading":            "正在载入…",
		"page.fail":          "无法载入页面：{err}",
		"copy":               "复制",
		"copied":             "已复制",
		"tr.from":            "译自{lang}",
		"tr.show":            "显示原文",
		"tr.back":            "显示译文",
		"post":               "发帖",
		"compose.note.post":  "用 Solana 钱包发帖：每帖签一次名，签的是消息，不是交易。",
		"compose.note.reply": "用 Solana 钱包回复：每条回复签一次名，签的是消息，不是交易。",
		"signin":             "用 Solana 登录",
		"yourpage":           "你的主页",
		"profile":            "资料…",
		"signout":            "退出",
		"re.clear":           "改为回复主帖",
		"placeholder.post":   "有什么新鲜事？",
		"placeholder.reply":  "写一条回复",
		"text.post":          "帖子内容",
		"text.reply":         "回复内容",
		"mention":            "提及",
		"checking":           "正在检查这个地址…",
		"choose.wallet":      "选择钱包：",
		"cancel":             "取消",
		"nowallet":           "这个浏览器里没有 Solana 钱包。",
		"declined":           "登录在钱包里被拒绝了。",
		"noconnect":          "钱包没有连接：{err}",
		"noaccount":          "没有账户",
		"banned":             "这把密钥已被禁止在这里发帖。",
		"below.post":         "这个地址持有的不足 {need}，只能阅读，不能发帖。",
		"below.reply":        "这个地址持有的不足 {need}，只能阅读，不能回复。",
		"unavailable":        "hub 暂时无法查询持仓，请稍后再试。",
		"gone":               "那条回复已被删除。清除它，改为回复主帖。",
		"again.post":         "{s} 秒后可以再发帖。",
		"again.reply":        "{s} 秒后可以再回复。",
		"each.post":          "每发一帖，钱包会请你签一次名。",
		"each.reply":         "每条回复，钱包会请你签一次名。",
		"over":               "超出 {max} 字节的上限 {n} 字节。",
		"check.fail":         "无法检查这个地址：{err}",
		"http":               "HTTP {code}",
		"units.raw":          "{n} 个最小单位",
		"units.tokens":       "{n} 个代币",
		"of.mint":            "{units}（mint {mint}）",
		"or":                 " 或 ",
		"changed":            "钱包在签名前改动了消息，hub 无法验证。",
		"youdeclined":        "你在钱包里拒绝了。",
		"nosign":             "钱包无法签名：{err}",
		"waiting":            "等待你的钱包…",
		"saving":             "正在存储…",
		"sending":            "正在发送…",
		"badsig":             "hub 无法验证钱包的签名。",
		"landed":             "这把密钥的另一条消息先到了。请再按一次「{btn}」。",
		"replying.pre":       "回复 ",
		"replying.post":      "",
		"posted":             "已发布。",
		"replied":            "已回复。",
		"profile.title":      "资料",
		"pic":                "头像",
		"choose":             "选择图片…",
		"name":               "名字",
		"display.name":       "显示名字",
		"holding":            "持仓",
		"edit":               "编辑",
		"ok":                 "好",
		"save":               "存储",
		"noname":             "还没有名字",
		"nogate":             "这个 hub 没有代币门槛。",
		"unknown":            "未知：hub 暂时无法读取持仓。",
		"admin":              "管理员密钥：不受发帖条件限制。",
		"enough":             "足以发帖。hub 要求 {need}。",
		"less":               "不足 hub 要求的 {need}，这个地址只能阅读，不能发帖。",
		"pic.max":            "图片最大 8 MB。",
		"pic.store":          "hub 暂时无法存储图片，请稍后再试。",
		"pic.type":           "hub 无法读取这个文件。请试试 PNG、JPEG 或 GIF。",
		"uploading":          "正在上传…",
		"pic.save":           "存储后使用这张图片。",
		"name.req":           "需要一个名字。",
		"name.max":           "名字最多 {n} 字节。",
		"saved":              "资料已存储。",
		"title.on":           "{name} · {host}",
		"stats":              "统计",
		"error.back":         "返回信息流。",
		"err.nopage":         "没有这个页面。",
		"err.feed":           "无法读取信息流。",
		"err.search":         "无法搜索帖子。",
		"err.ambiguous":      "不止一条帖子以此开头，请使用完整的 id。",
		"err.nopost":         "没有这条帖子。",
		"err.post":           "无法读取这条帖子。",
		"err.thread":         "无法读取这个主题。",
		"err.posts":          "无法读取帖子。",
		"err.noprofile":      "没有这个资料。",
		"err.profile":        "无法读取资料。",
		"err.nostats":        "这个 hub 没有统计。",
		"err.stats":          "无法读取统计。",
	},
	"ja": {
		"nothing":            "まだ何もありません。",
		"find":               "投稿を検索",
		"search":             "検索",
		"notify.off":         "新しい投稿を通知する",
		"notify.on":          "新しい投稿を通知中",
		"language":           "言語",
		"prev":               "前へ",
		"next":               "次へ",
		"members":            "人のメンバー",
		"posts":              "件の投稿",
		"online":             "人がオンライン",
		"online.title":       "この hub の読者",
		"matches":            "{n} 件の投稿が一致",
		"nomatch":            "一致する投稿はありません。",
		"search.hint":        "単語を 1 つか 2 つ入力して検索してください。すべての単語が投稿に含まれている必要があります。",
		"back":               "フィードに戻る",
		"feed":               "フィード",
		"replies":            "{n} 件の返信",
		"replies.range":      "{n} 件の返信のうち {a}–{b} 件目",
		"summary":            "要約",
		"summary.of":         "最初の {n} 件の返信の要約",
		"summarize":          "要約",
		"reply":              "返信",
		"inreply":            "返信先",
		"inreply.name":       "{name} への返信",
		"since.pre":          "参加日 ",
		"since.post":         "",
		"noposts":            "まだ投稿はありません。",
		"archived":           "アーカイブ",
		"picture":            "画像",
		"sound":              "音声",
		"page":               "ページ",
		"download":           "ダウンロード",
		"close":              "閉じる",
		"zoom":               "ズーム",
		"loading":            "読み込み中…",
		"page.fail":          "ページを読み込めませんでした：{err}",
		"copy":               "コピー",
		"copied":             "コピー済み",
		"tr.from":            "{lang}から翻訳",
		"tr.show":            "原文を表示",
		"tr.back":            "翻訳を表示",
		"post":               "投稿",
		"compose.note.post":  "Solana ウォレットで投稿：署名 1 回、トランザクションは不要です。",
		"compose.note.reply": "Solana ウォレットで返信：署名 1 回、トランザクションは不要です。",
		"signin":             "Solana でサインイン",
		"yourpage":           "あなたのページ",
		"profile":            "プロフィール…",
		"signout":            "サインアウト",
		"re.clear":           "スレッドの投稿に返信する",
		"placeholder.post":   "いまどうしてる？",
		"placeholder.reply":  "返信を書く",
		"text.post":          "投稿の本文",
		"text.reply":         "返信の本文",
		"mention":            "メンション",
		"checking":           "このアドレスを確認しています…",
		"choose.wallet":      "ウォレットを選択：",
		"cancel":             "キャンセル",
		"nowallet":           "このブラウザに Solana ウォレットがありません。",
		"declined":           "ウォレットでサインインが拒否されました。",
		"noconnect":          "ウォレットが接続しませんでした：{err}",
		"noaccount":          "アカウントがありません",
		"banned":             "この鍵はここへの投稿を禁止されています。",
		"below.post":         "このアドレスの保有量は {need} に満たないため、閲覧はできますが投稿はできません。",
		"below.reply":        "このアドレスの保有量は {need} に満たないため、閲覧はできますが返信はできません。",
		"unavailable":        "hub はいま保有量を確認できません。後でもう一度お試しください。",
		"gone":               "その返信は削除されました。解除してスレッドの投稿に返信してください。",
		"again.post":         "あと {s} 秒で投稿できます。",
		"again.reply":        "あと {s} 秒で返信できます。",
		"each.post":          "投稿ごとにウォレットで 1 回署名します。",
		"each.reply":         "返信ごとにウォレットで 1 回署名します。",
		"over":               "上限の {max} バイトを {n} バイト超えています。",
		"check.fail":         "このアドレスを確認できませんでした：{err}",
		"http":               "HTTP {code}",
		"units.raw":          "{n} 最小単位",
		"units.tokens":       "{n} トークン",
		"of.mint":            "{units}（mint {mint}）",
		"or":                 " または ",
		"changed":            "ウォレットが署名前にメッセージを変えたため、hub は検証できませんでした。",
		"youdeclined":        "ウォレットで拒否しました。",
		"nosign":             "ウォレットが署名できませんでした：{err}",
		"waiting":            "ウォレットを待っています…",
		"saving":             "保存中…",
		"sending":            "送信中…",
		"badsig":             "hub はウォレットの署名を検証できませんでした。",
		"landed":             "この鍵からの別のメッセージが先に届きました。「{btn}」をもう一度押してください。",
		"replying.pre":       "",
		"replying.post":      " への返信",
		"posted":             "投稿しました。",
		"replied":            "返信しました。",
		"profile.title":      "プロフィール",
		"pic":                "画像",
		"choose":             "画像を選択…",
		"name":               "名前",
		"display.name":       "表示名",
		"holding":            "保有量",
		"edit":               "編集",
		"ok":                 "OK",
		"save":               "保存",
		"noname":             "まだ名前がありません",
		"nogate":             "この hub にトークンの条件はありません。",
		"unknown":            "不明：hub はいま保有量を読めません。",
		"admin":              "管理者の鍵：条件は適用されません。",
		"enough":             "投稿できます。hub が求めるのは {need} です。",
		"less":               "hub が求める {need} に満たないため、このアドレスは閲覧はできますが投稿はできません。",
		"pic.max":            "画像は最大 8 MB です。",
		"pic.store":          "hub はいま画像を保存できません。後でもう一度お試しください。",
		"pic.type":           "hub が読める画像ではありません。PNG、JPEG、GIF をお試しください。",
		"uploading":          "アップロード中…",
		"pic.save":           "保存するとこの画像が使われます。",
		"name.req":           "名前が必要です。",
		"name.max":           "名前は最大 {n} バイトです。",
		"saved":              "プロフィールを保存しました。",
		"title.on":           "{host} の {name}",
		"stats":              "統計",
		"error.back":         "フィードに戻る。",
		"err.nopage":         "そのページはありません。",
		"err.feed":           "フィードを読めませんでした。",
		"err.search":         "投稿を検索できませんでした。",
		"err.ambiguous":      "その始まりの投稿が複数あります。id 全体を使ってください。",
		"err.nopost":         "その投稿はありません。",
		"err.post":           "投稿を読めませんでした。",
		"err.thread":         "スレッドを読めませんでした。",
		"err.posts":          "投稿を読めませんでした。",
		"err.noprofile":      "そのプロフィールはありません。",
		"err.profile":        "プロフィールを読めませんでした。",
		"err.nostats":        "この hub に統計はありません。",
		"err.stats":          "統計を読めませんでした。",
	},
}
