// Package render 负责把用户提交的 Markdown 变成可以直接塞进页面的 HTML，
// 同时产出用于摘要和全文索引的纯文本。
//
// 两条硬规则：
//   - 绝不开启 goldmark 的 Unsafe 选项。正文来自用户，原始 HTML 必须转义，
//     否则就是存储型 XSS。
//   - 所有指向站外的链接自动带上 rel="ugc nofollow noopener"。不这么做，
//     站点会迅速变成外链农场的宿主，这是搜索引擎最直接的降权理由之一。
package render

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Result 是一次渲染的全部产物。HTML 存库做缓存，Plain 用于摘要与 FTS。
type Result struct {
	HTML     string
	Plain    string
	Summary  string
	Words    int
	Headings []Heading // 文章大纲，取 h2/h3
}

// Renderer 复用同一个 goldmark 实例，并发安全。
type Renderer struct {
	md goldmark.Markdown
}

// SizeLookup 回答"站内这张图多大"。/media/ab/xxxx.webp 这样的站内路径
// 传进来，返回像素宽高。
//
// 传函数而不是传媒体表：render 不该知道数据库的存在。它只需要这一个答案。
type SizeLookup func(src string) (w, h int, ok bool)

// New 构造渲染器。siteHost 用来区分站内外链接（如 "cligc.com"），留空则
// 视所有绝对 URL 为站外。
func New(siteHost string, size SizeLookup) *Renderer {
	t := &ugcLinks{host: strings.ToLower(siteHost), size: size}
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			highlighting.NewHighlighting(
				// 关键：输出 class 而不是内联 style。
				//
				// chroma 默认给每个 token 写 style="color:#..."，而站点的 CSP 是
				// style-src 'self' —— 那些内联样式会被整段拦掉，代码块直接退化成
				// 一片白字，且浏览器不会报任何可见错误。用 class 之后配色由
				// app.css 决定，顺带也能跟着站点的明暗主题走。
				highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
				// 不猜语言：猜错了比不高亮更难看，而且猜测要跑一遍所有 lexer。
				highlighting.WithGuessLanguage(false),
				highlighting.WithWrapperRenderer(codeWrapper),
			),
		),
		goldmark.WithParserOptions(
			// WithAutoHeadingID 负责"给标题生成 ID"这件事本身；
			// 具体怎么生成由 Render 里传入的 parser.WithIDs 决定，
			// 因为 ID 生成器是**上下文**选项，作用域是单次解析。
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(util.Prioritized(t, 100)),
		),
	)
	return &Renderer{md: md}
}

// codeWrapper 包住每个代码块，把语言写进 data-lang，让 CSS 能在右上角
// 标出语言而不需要额外的 DOM 或内联样式。
//
// 语言标记来自用户写在 ``` 后面的那串字符，是不可信输入，必须转义。
func codeWrapper(w util.BufWriter, c highlighting.CodeBlockContext, entering bool) {
	// 被高亮的块由 chroma 自己写 <pre><code>；没有语言标记的块不经过 chroma，
	// 得由这里补上，否则纯文本代码块会丢掉 pre 语义。两种情况下外层
	// <div class="codeblock"> 都要有，CSS 才能统一处理。
	if !entering {
		if !c.Highlighted() {
			w.WriteString("</code></pre>")
		}
		w.WriteString("</div>")
		return
	}
	w.WriteString(`<div class="codeblock"`)
	if lang, ok := c.Language(); ok && len(lang) > 0 {
		w.WriteString(` data-lang="`)
		w.Write(util.EscapeHTML(lang))
		w.WriteString(`"`)
	}
	w.WriteString(`>`)
	if !c.Highlighted() {
		w.WriteString(`<pre class="chroma"><code>`)
	}
}

// Render 解析一次 Markdown，同时产出 HTML 与纯文本。
func (r *Renderer) Render(src string) Result {
	b := []byte(src)
	// 每次解析给一个全新的 ID 生成器：去重的作用域就是这一篇文章，
	// 跨文章共享会让第二篇的锚点莫名其妙带上 -2 后缀。
	ctx := parser.NewContext(parser.WithIDs(newCJKIDs()))
	doc := r.md.Parser().Parse(text.NewReader(b), parser.WithContext(ctx))

	var buf bytes.Buffer
	if err := r.md.Renderer().Render(&buf, b, doc); err != nil {
		// goldmark 的 HTML renderer 只在 writer 出错时返回 error，
		// bytes.Buffer 不会出错；真出错就退化成纯文本，不让一篇坏文章拖垮请求。
		esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(src)
		return Result{HTML: "<p>" + esc + "</p>", Plain: src, Summary: Summarize(src, 140), Words: CountWords(src)}
	}

	plain := plainText(doc, b)
	return Result{
		HTML:     buf.String(),
		Plain:    plain,
		Summary:  Summarize(plain, 140),
		Words:    CountWords(plain),
		Headings: collectHeadings(doc, b),
	}
}

// ugcLinks 是一个 AST transformer，给站外链接补上 rel / target 属性。
// goldmark 的 HTML renderer 会把 node 上的属性按 LinkAttributeFilter 输出，
// rel 与 target 都在白名单内。
type ugcLinks struct {
	host string
	size SizeLookup
}

func (t *ugcLinks) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	src := reader.Source()
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			if t.external(string(v.Destination)) {
				mark(v)
			}
		case *ast.AutoLink:
			if t.external(string(v.URL(src))) {
				mark(v)
			}
		case *ast.Image:
			t.markImage(v)
		}
		return ast.WalkContinue, nil
	})
}

// markImage 给正文里的图片补上三样东西：延迟加载、异步解码、像素尺寸。
//
// 尺寸是其中真正要紧的那个。没有 width/height，浏览器在图片下载完之前
// 不知道该留多高的位置，图一到位就把下面的正文整段往下顶——这是 Core
// Web Vitals 里 CLS 那一项最常见的来源，而长文里每张图都会顶一次。
//
// loading="lazy" 不加在第一张图上：首屏的图延迟加载反而会拖慢 LCP。
// 但正文里第一张图通常也不在首屏（上面还有标题和一段字），这里不为这个
// 例外增加判断——判断错的代价比收益大。
func (t *ugcLinks) markImage(n *ast.Image) {
	n.SetAttributeString("loading", []byte("lazy"))
	n.SetAttributeString("decoding", []byte("async"))
	if t.size == nil {
		return
	}
	dest := string(n.Destination)
	// 只查站内的图。站外图片的尺寸这里拿不到，也不该为了拿它去发请求——
	// 渲染一篇文章会变成对着别人的服务器打一串同步请求。
	if !strings.HasPrefix(dest, "/") {
		return
	}
	if w, h, ok := t.size(dest); ok && w > 0 && h > 0 {
		n.SetAttributeString("width", []byte(strconv.Itoa(w)))
		n.SetAttributeString("height", []byte(strconv.Itoa(h)))
	}
}

func mark(n ast.Node) {
	n.SetAttributeString("rel", []byte("ugc nofollow noopener"))
	n.SetAttributeString("target", []byte("_blank"))
}

// external 判断目标是否为站外。相对路径、锚点、mailto 都算站内（不加 rel）。
func (t *ugcLinks) external(dest string) bool {
	u, err := url.Parse(dest)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if t.host == "" {
		return true
	}
	return h != t.host && !strings.HasSuffix(h, "."+t.host)
}

// plainText 遍历 AST 抽出可读文本。用 AST 而不是"渲染完再剥标签"，
// 是为了让代码块、图片 alt 这些边界情况有确定行为。
func plainText(doc ast.Node, src []byte) string {
	var sb strings.Builder
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch v := n.(type) {
		case *ast.Text:
			if entering {
				sb.Write(v.Segment.Value(src))
				if v.SoftLineBreak() || v.HardLineBreak() {
					sb.WriteByte(' ')
				}
			}
		case *ast.String:
			if entering {
				sb.Write(v.Value)
			}
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			if entering {
				l := v.Lines()
				for i := 0; i < l.Len(); i++ {
					s := l.At(i)
					sb.Write(s.Value(src))
				}
			}
			return ast.WalkSkipChildren, nil
		}
		if !entering && n.Type() == ast.TypeBlock {
			sb.WriteByte('\n')
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(collapse(sb.String()))
}

// collapse 把连续空白压成单个空格，换行保留为空格。
func collapse(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		space = false
		sb.WriteRune(r)
	}
	return sb.String()
}

// Summarize 截取前 n 个字符作为摘要，尽量落在句末标点上。
func Summarize(s string, n int) string {
	s = collapse(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	cut := rs[:n]
	// 回退到最近的句末标点，避免把句子截半
	for i := len(cut) - 1; i > n*2/3; i-- {
		switch cut[i] {
		case '。', '！', '？', '.', '!', '?', '；', ';':
			return string(cut[:i+1])
		}
	}
	return strings.TrimSpace(string(cut)) + "…"
}

// CountWords 统计字数：CJK 按字算，拉丁按词算。中英混排时这个口径
// 比单纯的 len([]rune) 更接近"读者感知的篇幅"。
func CountWords(s string) int {
	n := 0
	inWord := false
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			n++
			inWord = false
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if !inWord {
				n++
				inWord = true
			}
		default:
			inWord = false
		}
	}
	return n
}
