// Package tokenize 把任意文本切成适合喂给 SQLite FTS5 unicode61 分词器的
// 空格分隔 token 串。
//
// 为什么需要它：FTS5 内置的 unicode61 把 CJK 字符归类为 alphanumeric，因此
// 一整段中文会被当成"一个 token"，导致中文检索完全失效。这里在写入前先做
// 一次 bigram（二元）切分，用空格隔开；unicode61 见到空格就断词，于是索引
// 里存的就是我们想要的中文二元组。查询端用同一套切分，并把连续 bigram 拼成
// FTS5 短语，从而拿到接近子串匹配的精度。
//
// 选 bigram 而不是词典分词（jieba/gse）是刻意的：零依赖、零词典、结果确定，
// 不会因为词典更新导致新旧索引不一致；代价是索引体积约为原文 2 倍，对内容站
// 完全可以接受。
package tokenize

import (
	"strings"
	"unicode"
)

// cjk 判断该 rune 是否属于需要做 bigram 的表意文字区（汉字、假名、谚文）。
func cjk(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// word 判断该 rune 是否属于拉丁/数字这类"按整词处理"的字符。
func word(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// run 是一段同类字符。
type run struct {
	text  string
	isCJK bool
}

// runs 把文本切成交替的 CJK 段与拉丁/数字段，丢弃其余分隔符。
func runs(s string) []run {
	var out []run
	var buf []rune
	var mode int // 0=none 1=cjk 2=word

	flush := func() {
		if len(buf) > 0 {
			out = append(out, run{text: string(buf), isCJK: mode == 1})
			buf = buf[:0]
		}
	}

	for _, r := range s {
		var m int
		switch {
		case cjk(r):
			m = 1
		case word(r):
			m = 2
		default:
			m = 0
		}
		if m != mode {
			flush()
			mode = m
		}
		if m != 0 {
			buf = append(buf, unicode.ToLower(r))
		}
	}
	flush()
	return out
}

// bigrams 把一段 CJK 文本切成二元组；单字则原样返回。
func bigrams(s string) []string {
	rs := []rune(s)
	if len(rs) == 0 {
		return nil
	}
	if len(rs) == 1 {
		return []string{string(rs)}
	}
	out := make([]string, 0, len(rs)-1)
	for i := 0; i+1 < len(rs); i++ {
		out = append(out, string(rs[i:i+2]))
	}
	return out
}

// Index 返回写入 FTS5 索引用的 token 串（空格分隔）。
func Index(s string) string {
	var toks []string
	for _, r := range runs(s) {
		if r.isCJK {
			toks = append(toks, bigrams(r.text)...)
		} else {
			toks = append(toks, r.text)
		}
	}
	return strings.Join(toks, " ")
}

// quote 按 FTS5 字符串字面量规则转义（双引号成对加倍）。
func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// Query 把用户输入的查询串转成 FTS5 MATCH 表达式。
//
// 每个 CJK 段落的连续 bigram 会被拼成一个短语，要求它们在索引里相邻出现，
// 等价于对该段做子串匹配；不同段落之间用 AND 连接。返回空串表示这个查询
// 没有任何可检索的内容，调用方应当直接返回空结果而不是执行 MATCH。
func Query(s string) string {
	var parts []string
	for _, r := range runs(s) {
		if r.isCJK {
			bg := bigrams(r.text)
			if len(bg) == 0 {
				continue
			}
			// 短语：要求 bigram 在索引中按序相邻
			parts = append(parts, quote(strings.Join(bg, " ")))
		} else {
			parts = append(parts, quote(r.text))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " AND ")
}

// Terms 返回查询串里的"原始词条"——CJK 连续段与拉丁词，均已小写，
// 不做 bigram 切分。检索结果做摘要高亮时需要的是这些原词，
// 而不是索引用的二元组。
func Terms(s string) []string {
	rs := runs(s)
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.text)
	}
	return out
}
