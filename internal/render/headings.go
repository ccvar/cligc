package render

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/util"
)

// Heading 是正文里的一个小标题，用于生成文章大纲。
type Heading struct {
	Level int    `json:"l"`
	ID    string `json:"i"`
	Text  string `json:"t"`
}

// cjkIDs 是 goldmark 的标题 ID 生成器，替换掉默认那个。
//
// 默认实现遇到多字节字符直接跳过，于是纯中文标题会产出空串并退化成
// "heading"、"heading-1"、"heading-2" 这样的按序编号。那意味着**在文章中间
// 插入一个新标题，它后面所有已经被分享出去的深链会全部静默失效**——
// 页面照常打开，只是跳错了位置，没有任何报错。
//
// 这里保留 CJK 字符，ID 直接由标题文字推导：`## 三条路` → `#三条路`。
// 现代浏览器在地址栏里会把它显示成中文，分享出去也可读；更重要的是它只跟
// 标题内容有关，重排、增删其它标题都不影响它。
type cjkIDs struct {
	used map[string]bool
}

func newCJKIDs() parser.IDs { return &cjkIDs{used: map[string]bool{}} }

func (s *cjkIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	base := slugForID(string(value))
	if base == "" {
		if kind == ast.KindHeading {
			base = "heading"
		} else {
			base = "id"
		}
	}
	cand := base
	for i := 2; s.used[cand]; i++ {
		cand = base + "-" + strconv.Itoa(i)
	}
	s.used[cand] = true
	return []byte(cand)
}

func (s *cjkIDs) Put(value []byte) { s.used[string(value)] = true }

// slugForID 把标题文字转成锚点 ID：保留字母数字和表意文字，其余压成连字符。
func slugForID(s string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			sb.WriteRune(unicode.ToLower(r))
			dash = false
		default:
			if !dash && sb.Len() > 0 {
				sb.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(sb.String(), "-")
}

// collectHeadings 从 AST 里取出 h2/h3 作为大纲。
//
// 只取这两级：h1 是文章标题本身（正文里不该再出现），h4 以下在大纲里
// 只会制造噪音——一份需要滚动才能看完的目录，没人会用。
func collectHeadings(doc ast.Node, src []byte) []Heading {
	var out []Heading
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok || h.Level < 2 || h.Level > 3 {
			return ast.WalkContinue, nil
		}
		id, ok := h.AttributeString("id")
		if !ok {
			return ast.WalkContinue, nil
		}
		text := strings.TrimSpace(string(h.Text(src)))
		if text == "" {
			return ast.WalkContinue, nil
		}
		out = append(out, Heading{
			Level: h.Level,
			ID:    string(util.EscapeHTML(toBytes(id))),
			Text:  text,
		})
		return ast.WalkSkipChildren, nil
	})
	return out
}

func toBytes(v any) []byte {
	switch t := v.(type) {
	case []byte:
		return t
	case string:
		return []byte(t)
	}
	return nil
}
