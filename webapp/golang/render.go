package main

import (
	"html/template"
	"strconv"
	"strings"
)

// Step19: 投稿リスト(posts.html + post.html)を html/template でなく手書きGoでレンダリングする。
// 目的は html/template.Execute の reflect.Value.Call(自動エスケープ呼び出し, pprof上 app の最大コスト)
// を投稿描画経路から排除すること。
//
// レギュレーション担保: 手書き出力が html/template の出力と「バイト完全一致」することを
// render_test.go が敵対的入力(<>&"' / 空 / 多バイト)で機械検証する（一致しなければビルド/テストが落ちる）。
// エスケープが要るのはユーザ投稿文(.Body / .Comment)のみ。他は検証済みの安全値:
//   .ID/.CommentCount=int, .CreatedAtFmt=ISO時刻, .User.AccountName=[0-9a-zA-Z_](validateUser),
//   .ImageURL=/image/N.ext, .CSRFToken=hex — いずれもHTML特殊文字を含まずエスケープしても不変。

// htmlTextReplacer は html/template の htmlReplacementTable と同一の実体参照化。
// 対象は NUL,",&,',+,<,> （html/template は HTML テキスト/引用属性コンテキストで + も &#43; に escape する）。
// 単一バイトキー・非再帰置換なので html/template の挙動と一致する（二重エスケープしない）。
var htmlTextReplacer = strings.NewReplacer(
	"\x00", "�",
	`"`, "&#34;",
	"&", "&amp;",
	"'", "&#39;",
	"+", "&#43;",
	"<", "&lt;",
	">", "&gt;",
)

func escHTML(s string) string {
	return htmlTextReplacer.Replace(s)
}

// renderPostInto は post.html テンプレートと同一バイト列を b へ書き込む。
func renderPostInto(b *strings.Builder, p *Post) {
	id := strconv.Itoa(p.ID)
	b.WriteString(`<div class="isu-post" id="pid_`)
	b.WriteString(id)
	b.WriteString(`" data-created-at="`)
	b.WriteString(escHTML(p.CreatedAtFmt))
	b.WriteString("\">\n  <div class=\"isu-post-header\">\n    <a href=\"/@")
	b.WriteString(p.User.AccountName)
	b.WriteString(" \" class=\"isu-post-account-name\">")
	b.WriteString(p.User.AccountName)
	b.WriteString("</a>\n    <a href=\"/posts/")
	b.WriteString(id)
	b.WriteString("\" class=\"isu-post-permalink\">\n      <time class=\"timeago\" datetime=\"")
	b.WriteString(escHTML(p.CreatedAtFmt))
	b.WriteString("\"></time>\n    </a>\n  </div>\n  <div class=\"isu-post-image\">\n    <img src=\"")
	b.WriteString(p.ImageURL)
	b.WriteString("\" class=\"isu-image\">\n  </div>\n  <div class=\"isu-post-text\">\n    <a href=\"/@")
	b.WriteString(p.User.AccountName)
	b.WriteString("\" class=\"isu-post-account-name\">")
	b.WriteString(p.User.AccountName)
	b.WriteString("</a>\n    ")
	b.WriteString(escHTML(p.Body))
	b.WriteString("\n  </div>\n  <div class=\"isu-post-comment\">\n    <div class=\"isu-post-comment-count\">\n      comments: <b>")
	b.WriteString(strconv.Itoa(p.CommentCount))
	b.WriteString("</b>\n    </div>\n\n    ")
	for i := range p.Comments {
		c := &p.Comments[i]
		b.WriteString("\n    <div class=\"isu-comment\">\n      <a href=\"/@")
		b.WriteString(c.User.AccountName)
		b.WriteString("\" class=\"isu-comment-account-name\">")
		b.WriteString(c.User.AccountName)
		b.WriteString("</a>\n      <span class=\"isu-comment-text\">")
		b.WriteString(escHTML(c.Comment))
		b.WriteString("</span>\n    </div>\n    ")
	}
	b.WriteString("\n    <div class=\"isu-comment-form\">\n      <form method=\"post\" action=\"/comment\">\n        <input type=\"text\" name=\"comment\">\n        <input type=\"hidden\" name=\"post_id\" value=\"")
	b.WriteString(id)
	b.WriteString("\">\n        <input type=\"hidden\" name=\"csrf_token\" value=\"")
	b.WriteString(p.CSRFToken)
	b.WriteString("\">\n        <input type=\"submit\" name=\"submit\" value=\"submit\">\n      </form>\n    </div>\n  </div>\n</div>\n")
}

// renderPosts は posts.html（中で post.html を range）と同一バイト列を返す。getIndex/getAccountName/getPosts 用。
func renderPosts(posts []Post) template.HTML {
	var b strings.Builder
	b.WriteString("<div class=\"isu-posts\">\n  ")
	for i := range posts {
		b.WriteString("\n  ")
		renderPostInto(&b, &posts[i])
		b.WriteString("\n  ")
	}
	b.WriteString("\n</div>\n")
	return template.HTML(b.String())
}

// renderPostOne は post_id.html の {{ template "post.html" .Post }} 相当（単一投稿）。
func renderPostOne(p Post) template.HTML {
	var b strings.Builder
	renderPostInto(&b, &p)
	return template.HTML(b.String())
}
