package lang

import "testing"

// TestFullWidth: a half-width mark against a Chinese character, or a
// comma jammed after a code span or a link, is set full-width, the space
// after it dropped; code spans, links and marks with no Chinese beside
// them are left alone. The first six are from the hub's own
// translations, where the rule was counted.
func TestFullWidth(t *testing.T) {
	for _, c := range [][2]string{
		{"只答一个光秃秃的 `zh`,66 次里出现 3 次", "只答一个光秃秃的 `zh`，66 次里出现 3 次"},
		{"打开 https://hub.v2core.com/?lang=zh,英文帖子会以简体中文呈现", "打开 https://hub.v2core.com/?lang=zh，英文帖子会以简体中文呈现"},
		{"`Wordless` 返回 true;在中文前加一个空格", "`Wordless` 返回 true；在中文前加一个空格"},
		{"具备这里需要的边界:`card.URL`", "具备这里需要的边界：`card.URL`"},
		{"做前缀匹配,安全吗?", "做前缀匹配，安全吗？"},
		{"按 Command-F,查找 `x`。", "按 Command-F，查找 `x`。"},
		// the space a half-width mark needed goes with it
		{"第一, 然后 then, 最后! 好", "第一，然后 then，最后！好"},
		{"中文: English words", "中文：English words"},
		{"`a`,`b`;`c` 都行", "`a`，`b`；`c` 都行"},
		// no Chinese against it: left as it is
		{"以 watcher 敲入的那行“Hub watcher,Livid 的自动化…”开头", "以 watcher 敲入的那行“Hub watcher,Livid 的自动化…”开头"},
		{"`a`, `b` and `host`:`port`", "`a`, `b` and `host`:`port`"},
		{"报的是 “table 1 has 1 columns, the post's 6”", "报的是 “table 1 has 1 columns, the post's 6”"},
		{"大约 10,000 个 token，下午 3:30 见，比例 16:9", "大约 10,000 个 token，下午 3:30 见，比例 16:9"},
		{"Hello, world! Really? Yes: it is; fine.", "Hello, world! Really? Yes: it is; fine."},
		{"| 语言 | 帖子 |\n| :--- | ---: |\n| en | 751 |", "| 语言 | 帖子 |\n| :--- | ---: |\n| en | 751 |"},
		// inside code and links nothing moves, whatever stands beside it
		{"用 `a,b;c:d?中文,好` 和 `?lang=zh`", "用 `a,b;c:d?中文,好` 和 `?lang=zh`"},
		{"见[文档,第二版](https://example.com/a,b;c?x=1:2)", "见[文档，第二版](https://example.com/a,b;c?x=1:2)"},
		{"链接https://example.com/a?b=c,d的说明", "链接https://example.com/a?b=c,d的说明"},
		// a face stays a face; already full-width stays
		{"好的:) 行;-) 嗯", "好的:) 行;-) 嗯"},
		{"已经是全角了，没问题：对吧？", "已经是全角了，没问题：对吧？"},
		{"", ""},
	} {
		got := FullWidth(c[0])
		if got != c[1] {
			t.Errorf("FullWidth(%q)\n  = %q\nwant %q", c[0], got, c[1])
		}
		if again := FullWidth(got); again != got {
			t.Errorf("not a fixed point: %q became %q", got, again)
		}
	}
}

// TestFullWidthJa: a half-width : ; ! ? against Japanese — Han or kana
// — is set full-width, the space after it dropped; a comma is left to
// the model, which writes 、 itself; code, links, times, faces and Latin
// stay. The first four are from the hub's own translations, where the
// habit was counted (13 colons in 6 of the first 22).
func TestFullWidthJa(t *testing.T) {
	for _, c := range [][2]string{
		{"アイデア:Hub の鐘に自分の鍵で署名する", "アイデア：Hub の鐘に自分の鍵で署名する"},
		{"公開ページのライブストリームへの第 2 層:黙って死ぬストリームを", "公開ページのライブストリームへの第 2 層：黙って死ぬストリームを"},
		{"このストリームの他の読み手には影響なし:onmessage ハンドラーは", "このストリームの他の読み手には影響なし：onmessage ハンドラーは"},
		{"なぜ今:メンションは今週", "なぜ今：メンションは今週"},
		{"本当? そう! いいえ;", "本当？そう！いいえ；"},
		{"やり方: `curl` で", "やり方：`curl` で"},
		{"`Wordless` は true;日本語の前", "`Wordless` は true；日本語の前"},
		// the comma is Japanese's own: left as written either way
		{"まず,次に、最後に", "まず,次に、最後に"},
		// no Japanese against it: left as it is
		{"Hello, world! Really? Yes: it is; fine.", "Hello, world! Really? Yes: it is; fine."},
		{"約 10,000 トークン、15:30 に、比率 16:9", "約 10,000 トークン、15:30 に、比率 16:9"},
		{"| 言語 | 投稿 |\n| :--- | ---: |\n| en | 751 |", "| 言語 | 投稿 |\n| :--- | ---: |\n| en | 751 |"},
		{"`a:b;c?日本語` と https://example.com/a:b?x=1 を見て", "`a:b;c?日本語` と https://example.com/a:b?x=1 を見て"},
		{"いいね:) はい;-) ええ", "いいね:) はい;-) ええ"},
		{"もう全角です：そうですね？", "もう全角です：そうですね？"},
		{"", ""},
	} {
		got := FullWidthJa(c[0])
		if got != c[1] {
			t.Errorf("FullWidthJa(%q)\n  = %q\nwant %q", c[0], got, c[1])
		}
		if again := FullWidthJa(got); again != got {
			t.Errorf("not a fixed point: %q became %q", got, again)
		}
	}
	if Tidy("en", "a:b 中:文") != "a:b 中:文" || Tidy("zh-Hans", "中:文") != "中：文" || Tidy("ja", "か:な") != "か：な" {
		t.Error("Tidy picks the wrong rule")
	}
}
