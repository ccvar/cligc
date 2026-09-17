package render

import "strings"
import "testing"

func TestExternalLinksGetUGCRel(t *testing.T) {
	r := New("cligc.com", nil)
	got := r.Render("[x](https://evil.example/spam) [y](/about) [z](https://cligc.com/p/1)").HTML
	if !strings.Contains(got, `rel="ugc nofollow noopener"`) {
		t.Fatalf("external link missing rel: %s", got)
	}
	if strings.Count(got, "ugc nofollow") != 1 {
		t.Fatalf("expected exactly 1 external link marked, got: %s", got)
	}
}

func TestRawHTMLIsEscaped(t *testing.T) {
	r := New("cligc.com", nil)
	got := r.Render(`<script>alert(1)</script>`).HTML
	if strings.Contains(got, "<script>") {
		t.Fatalf("raw HTML not escaped: %s", got)
	}
}

func TestPlainAndSummary(t *testing.T) {
	r := New("cligc.com", nil)
	res := r.Render("# 标题\n\n这是正文。还有第二句。\n\n```go\nfmt.Println(1)\n```")
	if !strings.Contains(res.Plain, "这是正文") || !strings.Contains(res.Plain, "fmt.Println") {
		t.Fatalf("plain text incomplete: %q", res.Plain)
	}
	if res.Words < 5 {
		t.Fatalf("word count too low: %d", res.Words)
	}
	if s := Summarize("一二三四五六七八九十。后面还有。", 12); s != "一二三四五六七八九十。" {
		t.Fatalf("summary = %q", s)
	}
}

// TestHeadingIDsSurviveReordering 守住中文锚点的稳定性。
//
// goldmark 默认的 ID 生成器遇到多字节字符直接跳过，纯中文标题因此退化成
// "heading"、"heading-1" 这样的按序编号。那意味着**在文章中间插入一个新标题，
// 它后面所有已经被分享出去的深链会全部静默错位**——页面照常打开，只是跳到了
// 别的段落，没有任何报错。
func TestHeadingIDsSurviveReordering(t *testing.T) {
	r := New("cligc.com", nil)

	before := r.Render("## 三条路\n\n正文\n\n## 选了第三条\n\n正文")
	// 在两个标题之间插入一节
	after := r.Render("## 三条路\n\n正文\n\n## 中间插进来的一节\n\n正文\n\n## 选了第三条\n\n正文")

	id := func(hs []Heading, text string) string {
		for _, h := range hs {
			if h.Text == text {
				return h.ID
			}
		}
		return ""
	}
	for _, title := range []string{"三条路", "选了第三条"} {
		a, b := id(before.Headings, title), id(after.Headings, title)
		if a == "" || b == "" {
			t.Fatalf("heading %q missing: before=%q after=%q", title, a, b)
		}
		if a != b {
			t.Errorf("插入新标题后 %q 的锚点从 %q 变成了 %q —— 已分享的深链会静默错位", title, a, b)
		}
	}
	// 锚点必须来自标题文字，不能是按序编号
	if strings.HasPrefix(id(before.Headings, "三条路"), "heading") {
		t.Error("中文标题退化成了按序编号的锚点")
	}
}

func TestHeadingCollection(t *testing.T) {
	r := New("cligc.com", nil)
	res := r.Render("# 不该出现在大纲里\n\n## 二级\n\n### 三级\n\n#### 四级太深\n\n正文")
	var got []string
	for _, h := range res.Headings {
		got = append(got, h.Text)
	}
	// h1 是文章标题本身，h4 以下在大纲里只会制造噪音
	if len(got) != 2 || got[0] != "二级" || got[1] != "三级" {
		t.Errorf("headings = %v, want only h2/h3", got)
	}
	// 重名标题要能区分
	dup := r.Render("## 同名\n\n正文\n\n## 同名\n\n正文")
	if len(dup.Headings) != 2 || dup.Headings[0].ID == dup.Headings[1].ID {
		t.Errorf("duplicate headings share an anchor: %+v", dup.Headings)
	}
}

// TestImagesGetLazyLoadingAndSize 正文里的图要带上延迟加载和像素尺寸。
//
// 尺寸是要紧的那个：没有 width/height，图片下载完会把下面的正文整段
// 往下顶，长文里每张图顶一次。
func TestImagesGetLazyLoadingAndSize(t *testing.T) {
	size := func(src string) (int, int, bool) {
		if src == "/media/ab/pic.webp" {
			return 1200, 800, true
		}
		return 0, 0, false
	}
	r := New("cligc.com", size)
	got := r.Render("![一张图](/media/ab/pic.webp)\n\n![外站图](https://example.com/x.png)\n\n![没登记](/media/zz/unknown.webp)").HTML

	for _, want := range []string{`loading="lazy"`, `decoding="async"`, `width="1200"`, `height="800"`} {
		if !strings.Contains(got, want) {
			t.Errorf("少了 %s：\n%s", want, got)
		}
	}
	// 站外图片不查尺寸——为此发请求会让渲染一篇文章变成对着别人的服务器
	// 打一串同步请求。
	if strings.Count(got, `width=`) != 1 {
		t.Errorf("给查不到尺寸的图也写了 width：\n%s", got)
	}
	// 但延迟加载三张都要有。
	if n := strings.Count(got, `loading="lazy"`); n != 3 {
		t.Errorf("loading=lazy 出现 %d 次，想要 3 次", n)
	}
}

// TestImageRenderingStaysEscaped 图片的 alt 和地址仍然是用户输入。
func TestImageRenderingStaysEscaped(t *testing.T) {
	r := New("cligc.com", nil)
	got := r.Render(`![" onerror="alert(1)](/media/a.webp)`).HTML
	if strings.Contains(got, `onerror="alert`) {
		t.Errorf("alt 里的引号没转义，属性被撑开了：\n%s", got)
	}
}
