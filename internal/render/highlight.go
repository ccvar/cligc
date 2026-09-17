package render

import (
	"html"
	"sort"
	"strings"
)

// Highlight 从 text 中截取一段包含查询词的窗口，并把命中处包上 <mark>。
//
// 为什么不用 FTS5 自带的 snippet()：索引里存的是 bigram token 串（见
// tokenize 包），snippet() 只能在那串二元组上工作，输出形如
// "这篇 篇文 [文章] 章讲" —— 每个字重复两遍，没法给人看。所以摘要
// 在原文上单独算。
//
// 返回值已做 HTML 转义，除了我们自己插入的 <mark>，可以安全地当作 HTML 输出。
func Highlight(text string, terms []string, window int) string {
	rs := []rune(text)
	lower := []rune(strings.ToLower(text))

	type span struct{ lo, hi int }
	var spans []span
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		tr := []rune(t)
		for i := 0; i+len(tr) <= len(lower); i++ {
			if string(lower[i:i+len(tr)]) == t {
				spans = append(spans, span{i, i + len(tr)})
			}
		}
	}

	// 没命中就退化成开头截断，调用方仍然拿得到可展示的摘要
	if len(spans) == 0 {
		return html.EscapeString(Summarize(text, window))
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })
	// 合并重叠区间，避免嵌套 <mark>
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s.lo <= last.hi {
			if s.hi > last.hi {
				last.hi = s.hi
			}
			continue
		}
		merged = append(merged, s)
	}

	// 窗口以第一处命中为中心
	start := merged[0].lo - window/3
	if start < 0 {
		start = 0
	}
	end := start + window
	if end > len(rs) {
		end = len(rs)
		if start = end - window; start < 0 {
			start = 0
		}
	}

	var sb strings.Builder
	if start > 0 {
		sb.WriteString("…")
	}
	pos := start
	for _, s := range merged {
		if s.hi <= start || s.lo >= end {
			continue
		}
		lo, hi := max(s.lo, start), min(s.hi, end)
		sb.WriteString(html.EscapeString(string(rs[pos:lo])))
		sb.WriteString("<mark>")
		sb.WriteString(html.EscapeString(string(rs[lo:hi])))
		sb.WriteString("</mark>")
		pos = hi
	}
	sb.WriteString(html.EscapeString(string(rs[pos:end])))
	if end < len(rs) {
		sb.WriteString("…")
	}
	return sb.String()
}
