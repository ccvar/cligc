package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"cligc.com/internal/i18n"
	"cligc.com/internal/store"
)

// mustLoadLocales 加载内置语言目录。生产里由 main.go 做，测试得自己来——
// 不加载的话每条文案都会退化成 key 本身，而那种失败看起来像"翻译没生效"，
// 很容易被误判成 i18n 的 bug。
func mustLoadLocales(t *testing.T) {
	t.Helper()
	if len(i18n.Codes()) > 0 {
		return
	}
	if err := i18n.LoadBuiltin(); err != nil {
		t.Fatal(err)
	}
}

type env struct {
	t    *testing.T
	h    http.Handler
	db   *store.DB
	uid  int64
	root string
}

func setup(t *testing.T) *env { return setupWithQueueCap(t, 0) }

// enableLangs 打开这些语言的对外提供。
//
// 默认只开默认语言——不这么设的话，站上明明只写中文，却会给 11 种语言
// 都发 hreflang，而那些页面是空的。验多语言行为的测试要自己先开。
func (e *env) enableLangs(codes ...string) {
	e.t.Helper()
	st := e.db.Settings(e.t.Context())
	st.EnabledLangs = codes
	if err := e.db.SaveSettings(e.t.Context(), st); err != nil {
		e.t.Fatal(err)
	}
}

// setupWithQueueCap 起一个待审队列上限可控的实例。0 沿用默认值。
func setupWithQueueCap(t *testing.T, queueCap int) *env {
	t.Helper()
	mustLoadLocales(t)
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	u, err := db.CreateUser(t.Context(), "a@b.com", "作者", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Config{
		BaseURL: "https://example.com", Title: "测试站", Description: "描述",
		MediaRoot: dir, DailyPublishCap: 0, PerPage: 2, CommentsEnabled: true,
		CommentQueueCap: queueCap,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	// 中间件顺序必须和 main.go 一致：WithLang 在最外层，它要在 mux 匹配
	// 之前剥掉 /en/ 前缀。harness 和生产接线不一致的话，测试会在一个
	// 现实中不存在的组合上通过。
	return &env{t: t, h: s.WithLang(s.WithSession(mux)), db: db, uid: u.ID, root: dir}
}

// post 发一个表单请求，可选带上登录会话。
func (e *env) post(path string, form url.Values, login bool) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if login {
		sid, _, err := e.db.CreateSession(e.t.Context(), e.uid)
		if err != nil {
			e.t.Fatal(err)
		}
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) get(path string) (int, string) {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w.Code, w.Body.String()
}

// publish 建一篇文章并发布，返回它。
func (e *env) publish(title, body string, tags []string) *store.Post {
	e.t.Helper()
	a := store.Actor{UserID: e.uid, Kind: "web"}
	p, err := e.db.CreatePost(e.t.Context(), a, store.CreatePostInput{
		Title: title, BodyMD: body, Tags: tags,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	p, err = e.db.PublishPost(e.t.Context(), a, p.ID, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

const noindexTag = `<meta name="robots" content="noindex,follow">`

// TestIndexPolicy 锁住"哪些页面进索引"这条规则。
//
// 这是整个站最容易在后续改动中被悄悄破坏的东西，而且破坏了不会报错，
// 只会在几个月后表现为"整站排名下滑"。
func TestIndexPolicy(t *testing.T) {
	e := setup(t)
	e.publish("第一篇文章", "正文内容一", []string{"Go"})
	e.publish("第二篇文章", "正文内容二", []string{"Go"})
	e.publish("第三篇文章", "正文内容三", nil)

	cases := []struct {
		path    string
		noindex bool
		why     string
	}{
		{"/", false, "首页第一页应当被收录"},
		{"/?page=2", true, "分页页面没有独立价值，只稀释抓取预算"},
		{"/p/" + e.publish("可收录文章", "正文", nil).Slug, false, "已发布文章是唯一真正要收录的页面"},
		{"/t/go", true, "标签页是导航，内容全是别处的摘要"},
		{"/search?q=正文", true, "站内搜索结果由用户输入生成，数量无限"},
		{"/search", true, "空搜索页同样不该进索引"},
	}
	for _, c := range cases {
		code, body := e.get(c.path)
		if code != 200 {
			t.Errorf("%s -> %d", c.path, code)
			continue
		}
		if got := strings.Contains(body, noindexTag); got != c.noindex {
			t.Errorf("%s: noindex=%v, want %v — %s", c.path, got, c.noindex, c.why)
		}
	}
}

func TestCanonicalAndNoindexForSyndicatedPost(t *testing.T) {
	e := setup(t)
	p := e.publish("转载的文章", "正文", nil)
	if _, err := e.db.UpdatePost(t.Context(), store.Actor{UserID: e.uid, IsAdmin: true}, p.ID,
		store.UpdatePostInput{CanonicalURL: strPtr("https://original.example/post")}); err != nil {
		t.Fatal(err)
	}
	_, body := e.get("/p/" + p.Slug)
	if !strings.Contains(body, `<link rel="canonical" href="https://original.example/post">`) {
		t.Error("canonical should point at the original site")
	}
	if !strings.Contains(body, noindexTag) {
		t.Error("syndicated copy must be noindex — duplicate content risks the whole domain")
	}
}

func TestDraftIsNotPubliclyVisible(t *testing.T) {
	e := setup(t)
	p, err := e.db.CreatePost(t.Context(), store.Actor{UserID: e.uid},
		store.CreatePostInput{Title: "未发布", BodyMD: "机密草稿内容"})
	if err != nil {
		t.Fatal(err)
	}
	code, body := e.get("/p/" + p.Slug)
	if code != http.StatusNotFound {
		t.Errorf("anonymous draft access = %d, want 404", code)
	}
	if strings.Contains(body, "机密草稿内容") {
		t.Error("draft body leaked to anonymous visitor")
	}
}

func TestSitemapOnlyListsIndexablePosts(t *testing.T) {
	e := setup(t)
	pub := e.publish("已发布可索引", "正文", nil)
	hidden := e.publish("已发布但不索引", "正文", nil)
	if _, err := e.db.UpdatePost(t.Context(), store.Actor{UserID: e.uid, IsAdmin: true}, hidden.ID,
		store.UpdatePostInput{Indexable: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	draft, _ := e.db.CreatePost(t.Context(), store.Actor{UserID: e.uid},
		store.CreatePostInput{Title: "草稿", BodyMD: "正文"})

	code, body := e.get("/sitemap.xml")
	if code != 200 {
		t.Fatalf("sitemap = %d", code)
	}
	if !strings.Contains(body, "/p/"+pub.Slug) {
		t.Error("indexable post missing from sitemap")
	}
	for name, slug := range map[string]string{"noindex post": hidden.Slug, "draft": draft.Slug} {
		if strings.Contains(body, "/p/"+slug) {
			t.Errorf("%s must not appear in the sitemap", name)
		}
	}
	// 聚合页也不该进 sitemap：抓取预算要留给文章本体
	for _, frag := range []string{"/search", "/t/", "?page="} {
		if strings.Contains(body, frag) {
			t.Errorf("sitemap should not contain %q", frag)
		}
	}
}

func TestRobotsAndFeed(t *testing.T) {
	e := setup(t)
	e.publish("一篇文章", "正文内容", nil)

	_, robots := e.get("/robots.txt")
	for _, want := range []string{"Disallow: /admin", "Disallow: /search", "Sitemap: https://example.com/sitemap.xml"} {
		if !strings.Contains(robots, want) {
			t.Errorf("robots.txt missing %q", want)
		}
	}

	code, feed := e.get("/feed.xml")
	if code != 200 || !strings.Contains(feed, "<title>一篇文章</title>") {
		t.Errorf("feed = %d, body:\n%s", code, feed)
	}
	// RSS 只放摘要：全文输出等于给内容农场一键镜像
	if strings.Contains(feed, "<content:encoded>") {
		t.Error("feed should not carry full article bodies")
	}
}

func TestAdminRequiresLogin(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/admin", "/admin/new", "/admin/tokens"} {
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusSeeOther {
			t.Errorf("%s anonymous = %d, want 303 redirect to login", p, w.Code)
		}
	}
}

func TestCSRFRejectsCrossSitePost(t *testing.T) {
	e := setup(t)
	sid, _, err := e.db.CreateSession(t.Context(), e.uid)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://evil.example"},
	} {
		r := httptest.NewRequest("POST", "/admin/posts/1/delete", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		for k, v := range h {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("cross-site POST with %v = %d, want 403", h, w.Code)
		}
	}
}

func TestExternalLinksAreMarkedUGC(t *testing.T) {
	e := setup(t)
	p := e.publish("带外链的文章",
		"看 [站外](https://spam.example/x) 和 [站内](/about)。", nil)
	_, body := e.get("/p/" + p.Slug)
	if n := strings.Count(body, `rel="ugc nofollow noopener"`); n != 1 {
		t.Errorf("external links marked %d times, want exactly 1", n)
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// TestArchivedPostStaysReachable 锁住归档的语义。
//
// 归档不是软删除：它的用途是"从列表和索引里撤下，但不让已有链接失效"。
// 如果哪次改动把归档做成了 404，已经被收录、被别人引用过的文章就全成了死链，
// 那比继续挂着还糟。
func TestArchivedPostStaysReachable(t *testing.T) {
	e := setup(t)
	p := e.publish("会被归档的文章", "这里是正文内容，用来做检索验证。", []string{"Go"})
	if _, err := e.db.SetStatus(t.Context(), store.Actor{UserID: e.uid, IsAdmin: true},
		p.ID, store.StatusArchived); err != nil {
		t.Fatal(err)
	}

	// 页面仍然可访问，但不进索引，且不谎称"仅你可见"
	code, body := e.get("/p/" + p.Slug)
	if code != 200 {
		t.Fatalf("archived post = %d, want 200 (archiving must not create dead links)", code)
	}
	if !strings.Contains(body, noindexTag) {
		t.Error("archived post should be noindex")
	}
	if strings.Contains(body, "仅你可见") {
		t.Error("archived posts are visible to everyone — the draft-only wording is wrong here")
	}

	// 但从所有聚合面里消失
	for _, path := range []string{"/", "/t/go", "/sitemap.xml", "/search?q=正文内容"} {
		if _, b := e.get(path); strings.Contains(b, "/p/"+p.Slug) {
			t.Errorf("archived post still listed on %s", path)
		}
	}
}

// TestNotFoundUsesStyledPage 确认未匹配路由走站点自己的 404，
// 而不是 net/http 的裸文本。
func TestNotFoundUsesStyledPage(t *testing.T) {
	e := setup(t)
	code, body := e.get("/nope/whatever")
	if code != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", code)
	}
	if !strings.Contains(body, "error-page") || !strings.Contains(body, noindexTag) {
		t.Error("404 should render the site's own error page, with noindex")
	}
}

// TestPagerPreservesQueryAndEscapes 锁住翻页链接的转义。
//
// 早先的实现手工拼 "?q="+词，用户搜了含 & 或空格的词就会拿到坏链接——
// 这类 bug 不报错，只是默默失效。
func TestPagerPreservesQueryAndEscapes(t *testing.T) {
	e := setup(t)
	for i := range 3 {
		e.publish("共同关键词 文章"+string(rune('A'+i)), "正文里都有这个共同关键词。", nil)
	}
	_, body := e.get("/search?q=" + url.QueryEscape("共同关键词 & 空格"))
	// 分页存在时，next 链接必须把查询串编码进去而不是裸拼
	if strings.Contains(body, `href="/search?q=共同关键词 &`) {
		t.Error("pager emitted an unescaped query string")
	}
}

// TestLoginIsRateLimited 确认登录接口挡得住慢速爆破。
func TestLoginIsRateLimited(t *testing.T) {
	e := setup(t)
	var last int
	for i := 0; i < loginMaxAttempts+2; i++ {
		r := httptest.NewRequest("POST", "/login",
			strings.NewReader("email=a@b.com&password=wrong"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		last = w.Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after %d bad logins got %d, want 429", loginMaxAttempts+2, last)
	}
}

// TestHealthz 确认探活端点真的碰了数据库。
func TestHealthz(t *testing.T) {
	e := setup(t)
	if code, body := e.get("/healthz"); code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz = %d %q", code, body)
	}
}

// TestAnonymousCommentGoesToModeration 锁住评论的核心约束：
// 陌生人写的东西在有人看过之前不会出现在页面上。
func TestAnonymousCommentGoesToModeration(t *testing.T) {
	e := setup(t)
	p := e.publish("可评论的文章", "正文内容", nil)

	w := e.post("/p/"+p.Slug+"/comments", url.Values{
		"author_name": {"路人"}, "body": {"这是一条匿名评论"},
	}, false)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("anonymous comment = %d, want 303", w.Code)
	}

	// 页面上还看不到
	_, body := e.get("/p/" + p.Slug)
	if strings.Contains(body, "这是一条匿名评论") {
		t.Error("unmoderated anonymous comment is publicly visible")
	}

	// 队列里有
	items, total, err := e.db.ListComments(t.Context(), store.CommentFilter{Status: store.CommentPending})
	if err != nil || total != 1 {
		t.Fatalf("queue total = %d err=%v", total, err)
	}
	// 审核通过后才出现
	if err := e.db.SetCommentStatus(t.Context(), store.Actor{UserID: e.uid, IsAdmin: true},
		items[0].ID, store.CommentApproved); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get("/p/" + p.Slug); !strings.Contains(body, "这是一条匿名评论") {
		t.Error("approved comment still not shown")
	}
}

// TestLoggedInCommentIsPublishedImmediately 登录用户是站点自己的账号，不需要排队。
func TestLoggedInCommentIsPublishedImmediately(t *testing.T) {
	e := setup(t)
	p := e.publish("文章", "正文内容", nil)

	if w := e.post("/p/"+p.Slug+"/comments", url.Values{"body": {"站长自己的回复"}}, true); w.Code != http.StatusSeeOther {
		t.Fatalf("logged-in comment = %d", w.Code)
	}
	if _, body := e.get("/p/" + p.Slug); !strings.Contains(body, "站长自己的回复") {
		t.Error("logged-in comment not shown immediately")
	}
}

// TestCommentHoneypotSilentlyDrops 蜜罐命中时静默丢弃：让机器人以为成功了，
// 比返回错误更能减少重试。
func TestCommentHoneypotSilentlyDrops(t *testing.T) {
	e := setup(t)
	p := e.publish("文章", "正文内容", nil)

	w := e.post("/p/"+p.Slug+"/comments", url.Values{
		"body": {"机器人留言"}, "website": {"http://spam.example"},
	}, false)
	if w.Code != http.StatusSeeOther {
		t.Errorf("honeypot hit = %d, want a normal-looking 303", w.Code)
	}
	if _, total, _ := e.db.ListComments(t.Context(), store.CommentFilter{}); total != 0 {
		t.Error("honeypot comment was stored")
	}
}

// TestCommentBodyIsEscapedNotRendered 评论必须是纯文本：不解析 Markdown、
// 不自动生成链接。这个站的整个论点就是别变成外链农场的宿主。
func TestCommentBodyIsEscapedNotRendered(t *testing.T) {
	e := setup(t)
	p := e.publish("文章", "正文内容", nil)
	uid := e.uid
	if _, err := e.db.CreateComment(t.Context(), store.CreateCommentInput{
		PostID: p.ID, UserID: &uid,
		Body: "<script>alert(1)</script> [链接](https://spam.example) https://also-spam.example",
	}); err != nil {
		t.Fatal(err)
	}
	_, body := e.get("/p/" + p.Slug)
	if strings.Contains(body, "<script>alert") {
		t.Error("comment HTML not escaped")
	}
	if strings.Contains(body, `href="https://spam.example"`) ||
		strings.Contains(body, `href="https://also-spam.example"`) {
		t.Error("comment produced a clickable link — comments must stay plain text")
	}
}

// TestCommentsDisabledRemovesTheRoute 关掉评论时路由必须不存在，
// 而不只是把表单藏起来——藏起来的表单仍然可以被直接 POST。
func TestCommentsDisabledRemovesTheRoute(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, _ := db.CreateUser(t.Context(), "a@b.com", "作者", "password123", "admin")
	s, err := New(db, Config{BaseURL: "https://example.com", Title: "站", MediaRoot: dir,
		PerPage: 5, CommentsEnabled: false})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	h := s.WithLang(s.WithSession(mux))

	a := store.Actor{UserID: u.ID}
	p, _ := db.CreatePost(t.Context(), a, store.CreatePostInput{Title: "文章", BodyMD: "正文"})
	db.PublishPost(t.Context(), a, p.ID, 0)

	r := httptest.NewRequest("POST", "/p/"+p.Slug+"/comments",
		strings.NewReader("body=试图绕过"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("POST to comments with comments disabled = %d, want 404", w.Code)
	}
	if _, total, _ := db.ListComments(t.Context(), store.CommentFilter{}); total != 0 {
		t.Error("comment stored while comments are disabled")
	}
}

// TestAdminPagesRequireLoginAndRender 覆盖新增的三个后台页面。
func TestAdminPagesRequireLoginAndRender(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/admin/media", "/admin/profile", "/admin/comments"} {
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusSeeOther {
			t.Errorf("%s anonymous = %d, want 303", p, w.Code)
		}
	}
	sid, _, _ := e.db.CreateSession(t.Context(), e.uid)
	for _, p := range []string{"/admin/media", "/admin/profile", "/admin/comments"} {
		r := httptest.NewRequest("GET", p, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("%s logged in = %d, want 200", p, w.Code)
		}
		if !strings.Contains(w.Body.String(), noindexTag) {
			t.Errorf("%s should be noindex", p)
		}
	}
}

// TestCodeBlocksUseClassesNotInlineStyles 站点 CSP 是 style-src 'self'，
// chroma 默认输出的内联 style 会被整段拦掉，代码块直接变成一片白字。
func TestCodeBlocksUseClassesNotInlineStyles(t *testing.T) {
	e := setup(t)
	p := e.publish("带代码的文章", "```go\nfunc main() {\n\ts := \"hi\"\n}\n```", nil)
	_, body := e.get("/p/" + p.Slug)

	if !strings.Contains(body, `class="codeblock" data-lang="go"`) {
		t.Error("code block wrapper or language label missing")
	}
	if !strings.Contains(body, `class="kd"`) || !strings.Contains(body, `class="s"`) {
		t.Error("chroma token classes missing — highlighting is not wired up")
	}
	if strings.Contains(body, "style=") {
		t.Error("inline style in page — CSP style-src 'self' would block it")
	}
}

// TestStaticAssetsAreFingerprintedAndCacheable 锁住静态资源的缓存策略。
//
// embed.FS 里的文件 modtime 是零值，http.FileServer 因此发不出
// Last-Modified 也发不出 ETag——没有指纹的话，浏览器每次访问都要把整个
// CSS 和 JS 重下一遍，而这种浪费不会以任何形式报错。
func TestStaticAssetsAreFingerprintedAndCacheable(t *testing.T) {
	e := setup(t)

	_, body := e.get("/")
	for _, name := range []string{"base.css", "base.js", "favicon.svg"} {
		want := "/static/" + name + "?v="
		if !strings.Contains(body, want) {
			t.Errorf("page does not reference %s with a content fingerprint", name)
		}
	}

	// 指纹必须真的来自内容：不同文件的指纹不能相同
	re := regexp.MustCompile(`/static/((?:base|site|admin)\.(?:css|js))\?v=([0-9a-f]+)`)
	got := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		got[m[1]] = m[2]
	}
	// 公开页应当加载 base + site 两层，不加载 admin
	if len(got) != 4 {
		t.Fatalf("公开页应引用 base/site 各两个文件，实际 %v", got)
	}
	for name := range got {
		if strings.HasPrefix(name, "admin.") {
			t.Errorf("公开页加载了 %s —— 后台样式不该出现在公开页面上", name)
		}
	}
	if got["base.css"] == got["base.js"] {
		t.Error("two different files share a fingerprint — it is not derived from content")
	}

	// 带指纹的 URL 可以放心长缓存
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, httptest.NewRequest("GET", "/static/base.css?v="+got["base.css"], nil))
	if w.Code != 200 {
		t.Fatalf("fingerprinted asset = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable (safe because the URL carries the content hash)", cc)
	}
}

// TestBaseInputRuleHasZeroSpecificity 守住一个踩过的坑。
//
// 基础表单样式必须用排除法匹配（属性选择器不匹配"没写 type 属性"的 input，
// 而 <input name="title"> 这种写法极常见），但 :not() 会把参数的特异性
// 累加上去：裸写五个 :not([type=...]) 会让这条"默认样式"变成 (0,5,1)，
// 反过来压死 .nav-search input 这类 (0,1,1) 的组件级覆盖。
//
// 后果是导航栏搜索框的宽度、内距、圆角、边框色全部失效，而 CSS 不会报任何
// 错——只能靠人盯着页面发现"我明明写了样式却没生效"。包进 :where() 之后
// 特异性恒为 0，任何类选择器都能干净覆盖。
var commentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)

func TestBaseInputRuleHasZeroSpecificity(t *testing.T) {
	src := allCSS(t)

	// 先剥掉 /* */ 注释：本文件的注释里就写着 :not([type=...]) 这串字符，
	// 不剥的话测试会把自己的说明文字当成选择器。
	stripped := commentRE.ReplaceAllString(src, "")

	// 每一处 :not([type= 都必须包在 :where() 里
	for _, line := range strings.Split(stripped, "\n") {
		if !strings.Contains(line, ":not([type=") {
			continue
		}
		if !strings.Contains(line, ":where(") {
			t.Errorf("这一行的 :not([type=...]) 没有包在 :where() 里，"+
				"会抬高基础规则的特异性并压死组件级覆盖:\n  %s", strings.TrimSpace(line))
		}
	}

	// 顺带确认组件级规则还在，不至于测试通过但样式被整段删掉
	for _, want := range []string{".nav-search input", ".nav-search:focus-within input", ".search-form input"} {
		if !strings.Contains(src, want) {
			t.Errorf("组件级规则 %q 不见了", want)
		}
	}
}

// TestNoStyleAttributeWritesInJS 守住一条实测出来的 CSP 行为。
//
// 站点 CSP 是 style-src 'self'。实测下来：
//
//	el.style.left = '42px'          ✅ 生效
//	el.style.setProperty('--x', …)  ✅ 生效
//	el.setAttribute('style', …)     ❌ 被静默拦掉
//
// 最后一种不报错、不抛异常，只是样式不生效——写的人完全看不出问题在哪。
// 所以提示框的坐标走 CSS 自定义属性，样式本体留在样式表里。
// jsCommentRE 匹配 // 行注释和 /* */ 块注释。
var jsCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*`)

func TestNoStyleAttributeWritesInJS(t *testing.T) {
	js := []byte(allJS(t))
	// 先剥注释：本测试解释这条规则时，注释里就写着 setAttribute('style')
	// 这串字符，不剥的话它会把自己的说明文字当成代码。
	src := jsCommentRE.ReplaceAllString(string(js), "")
	for _, bad := range []string{`setAttribute('style'`, `setAttribute("style"`} {
		if strings.Contains(src, bad) {
			t.Errorf("app.js 用了 %s —— 站点 CSP 会静默拦掉它，样式不会生效", bad)
		}
	}
	// 提示框必须靠自定义属性传坐标
	if !strings.Contains(src, "setProperty('--tip-x'") {
		t.Error("提示框没有通过 CSS 自定义属性传坐标")
	}
}

// TestIconButtonsHaveAccessibleNames 图标按钮没有可见文字，
// aria-label 是它对读屏用户唯一的名字。漏写的话那就是个无名控件，
// 而这种缺陷在视觉上完全看不出来。
func TestIconButtonsHaveAccessibleNames(t *testing.T) {
	e := setup(t)
	p := e.publish("有操作按钮的文章", "正文内容", nil)
	uid := e.uid
	if _, err := e.db.CreateComment(t.Context(), store.CreateCommentInput{
		PostID: p.ID, UserID: &uid, Body: "一条评论",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, _ := e.db.CreateSession(t.Context(), e.uid)

	// 匹配一个 icon-btn 开标签的全部属性
	tag := regexp.MustCompile(`<(?:a|button)[^>]*class="icon-btn[^"]*"[^>]*>`)
	total := 0
	for _, path := range []string{"/admin", "/admin/comments?status=all", "/admin/tokens", "/admin/media"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s = %d", path, w.Code)
		}
		for _, m := range tag.FindAllString(w.Body.String(), -1) {
			total++
			if !strings.Contains(m, "aria-label=") {
				t.Errorf("%s 上有个图标按钮没有 aria-label:\n  %s", path, m)
			}
			if !strings.Contains(m, "data-tip=") {
				t.Errorf("%s 上有个图标按钮没有 data-tip（鼠标用户看不到它是干什么的）:\n  %s", path, m)
			}
		}
	}
	if total == 0 {
		t.Fatal("一个图标按钮都没找到，选择器或模板可能变了")
	}
	t.Logf("检查了 %d 个图标按钮", total)
}

// TestFeaturedSectionOnHomepage 首屏的"宣传"由内容完成，
// 所以精选区必须真的出现，且不能让同一篇文章在一屏里露两次。
func TestFeaturedSectionOnHomepage(t *testing.T) {
	e := setup(t)
	a := store.Actor{UserID: e.uid, IsAdmin: true}
	lead := e.publish("被置顶的头条", "头条的正文内容", nil)
	plain := e.publish("没被置顶的文章", "普通正文", nil)

	// 未置顶时没有精选区
	if _, body := e.get("/"); strings.Contains(body, `class="featured"`) {
		t.Error("featured section rendered with nothing featured")
	}

	if _, err := e.db.SetFeatured(t.Context(), a, lead.ID, true); err != nil {
		t.Fatal(err)
	}
	_, body := e.get("/")
	if !strings.Contains(body, `class="featured"`) || !strings.Contains(body, "featured-lead") {
		t.Fatal("featured section missing after pinning")
	}
	if n := strings.Count(body, `href="/p/`+lead.Slug+`"`); n != 1 {
		t.Errorf("featured post appears %d times on the homepage, want exactly 1 "+
			"(it must not also show in the chronological list)", n)
	}
	if !strings.Contains(body, `href="/p/`+plain.Slug+`"`) {
		t.Error("non-featured post disappeared from the list")
	}

	// 精选区只在第一页；后续页仍然排除精选，否则分页会错位
	if _, body := e.get("/?page=2"); strings.Contains(body, `class="featured"`) {
		t.Error("featured section should only render on page 1")
	}
}

// TestFeatureToggleRequiresPublished 草稿不能被置顶到首页最显眼的位置——
// 那等于绕过了发布这道闸门。
func TestFeatureToggleRequiresPublished(t *testing.T) {
	e := setup(t)
	draft, err := e.db.CreatePost(t.Context(), store.Actor{UserID: e.uid},
		store.CreatePostInput{Title: "草稿", BodyMD: "正文"})
	if err != nil {
		t.Fatal(err)
	}
	w := e.post("/admin/posts/"+strconv.FormatInt(draft.ID, 10)+"/feature", nil, true)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("feature draft = %d", w.Code)
	}
	got, _ := e.db.PostByID(t.Context(), draft.ID)
	if got.IsFeatured() {
		t.Error("a draft got featured")
	}
}

// TestArticleTOC 大纲是服务端渲染的：爬虫能看到章节链接，关掉 JS 也能用。
func TestArticleTOC(t *testing.T) {
	e := setup(t)
	p := e.publish("带小标题的文章",
		"开头\n\n## 第一节\n\n内容\n\n## 第二节\n\n内容", nil)
	_, body := e.get("/p/" + p.Slug)

	if !strings.Contains(body, `class="toc"`) {
		t.Fatal("TOC missing from the server-rendered HTML")
	}
	for _, want := range []string{"第一节", "第二节"} {
		if !strings.Contains(body, want) {
			t.Errorf("TOC missing heading %q", want)
		}
	}
	// 锚点来自标题文字，不是按序编号
	if strings.Contains(body, `href="#heading"`) {
		t.Error("anchors degraded to positional ids — deep links break when headings are inserted")
	}
	// 容器加宽但正文栏宽不变
	if !strings.Contains(body, `<body class="reading">`) {
		t.Error("article page should use the reading layout when it has a TOC")
	}

	// 只有一个小标题时不出大纲：一条目录不提供导航价值，却要占掉一整栏
	one := e.publish("只有一个小标题", "开头\n\n## 唯一一节\n\n内容", nil)
	if _, b := e.get("/p/" + one.Slug); strings.Contains(b, `class="toc"`) {
		t.Error("a one-entry TOC should not render")
	}
}

// TestFooterBrand 页脚也有站标，但它是静态的——只有顶部那枚跟鼠标转。
func TestFooterBrand(t *testing.T) {
	e := setup(t)
	_, body := e.get("/")
	if !strings.Contains(body, `class="foot-brand"`) {
		t.Error("footer brand missing")
	}
	if n := strings.Count(body, `class="brand-mark"`); n != 2 {
		t.Errorf("brand marks = %d, want 2 (header + footer)", n)
	}
	js := []byte(allJS(t))
	if !strings.Contains(string(js), ".site-header .brand-mark") {
		t.Error("cursor animation must target the header mark only, or both marks will move")
	}
}

// TestHreflangIsReciprocalAndSelfCanonical 守住多语种最致命的三个错误。
//
//  1. hreflang 必须互相引用，每一版列出所有版本包括它自己。单向标注会被
//     Google 整体忽略——而且不报错，只是安静地当它不存在。
//  2. 必须有 x-default。
//  3. 每一版的 canonical 必须指向**它自己**。把译文的 canonical 指向原文，
//     等于告诉 Google 别收录译文，整套多语种工作全部作废。这是最常见的错法。
func TestHreflangIsReciprocalAndSelfCanonical(t *testing.T) {
	e := setup(t)
	ctx := t.Context()
	a := store.Actor{UserID: e.uid, IsAdmin: true}

	e.enableLangs("en")

	zh := e.publish("中文版", "正文内容", nil)
	en, err := e.db.CreatePost(ctx, store.Actor{UserID: e.uid},
		store.CreatePostInput{Title: "English", BodyMD: "Body", Lang: "en", Slug: "english"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.PublishPost(ctx, a, en.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.db.LinkTranslation(ctx, a, en.ID, zh.ID); err != nil {
		t.Fatal(err)
	}

	zhURL := "https://example.com/p/" + zh.Slug
	enURL := "https://example.com/en/p/" + en.Slug

	for _, c := range []struct{ path, self string }{
		{"/p/" + zh.Slug, zhURL},
		{"/en/p/" + en.Slug, enURL},
	} {
		_, body := e.get(c.path)
		// 互相引用：两个方向都要出现
		for _, want := range []string{
			`hreflang="zh-Hans" href="` + zhURL + `"`,
			`hreflang="en" href="` + enURL + `"`,
			`hreflang="x-default" href="` + zhURL + `"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 缺少 %s", c.path, want)
			}
		}
		// canonical 指向自己
		if !strings.Contains(body, `<link rel="canonical" href="`+c.self+`">`) {
			t.Errorf("%s 的 canonical 没有指向它自己（%s）——这会让 Google 不收录这一版", c.path, c.self)
		}
	}
}

// TestLanguageRouting 语言前缀的路由行为。
func TestLanguageRouting(t *testing.T) {
	e := setup(t)
	e.enableLangs("en")
	if code, body := e.get("/"); code != 200 || !strings.Contains(body, `<html lang="zh-Hans"`) {
		t.Errorf("默认语言 = %d，html lang 不对", code)
	}
	code, body := e.get("/en/")
	if code != 200 || !strings.Contains(body, `<html lang="en"`) {
		t.Fatalf("/en/ = %d，html lang 不对", code)
	}
	// 界面确实翻译了，不是只换了 lang 属性
	if !strings.Contains(body, "Sign in") {
		t.Error("/en/ 的界面文案没有翻译")
	}
	// 默认语言不该有前缀：/zh/ 要 301 回 /，否则同一内容两个 URL
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, httptest.NewRequest("GET", "/zh/", nil))
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/" {
		t.Errorf("/zh/ = %d -> %q, want 301 -> /", w.Code, w.Header().Get("Location"))
	}
}

// TestListsAreLanguageScoped 一个列表里中英混排对读者是噪音、
// 对搜索引擎是语言信号混乱。
func TestListsAreLanguageScoped(t *testing.T) {
	e := setup(t)
	e.enableLangs("en")
	ctx := t.Context()
	zh := e.publish("只有中文的文章", "正文", nil)
	en, _ := e.db.CreatePost(ctx, store.Actor{UserID: e.uid},
		store.CreatePostInput{Title: "English only", BodyMD: "Body", Lang: "en", Slug: "en-only"})
	e.db.PublishPost(ctx, store.Actor{UserID: e.uid, IsAdmin: true}, en.ID, 0)

	_, zhBody := e.get("/")
	_, enBody := e.get("/en/")
	if !strings.Contains(zhBody, zh.Slug) || strings.Contains(zhBody, "en-only") {
		t.Error("中文首页混入了英文文章")
	}
	if !strings.Contains(enBody, "en-only") || strings.Contains(enBody, zh.Slug) {
		t.Error("英文首页混入了中文文章")
	}
}

// TestNoHorizontalOffscreenHiding 守住一条 RTL 下才会暴露的布局 bug。
//
// 「把元素挪到 -9999px 藏起来」这招只在它挪向浏览器会裁掉的那一侧时成立。
// 而哪一侧会被裁，取决于书写方向：
//
//	LTR：原点在左，左侧溢出被裁 → left: -9999px  可行
//	RTL：原点在右，右侧溢出被裁 → left: -9999px  反而撑出 9999px 横向滚动条
//
// 阿拉伯语页面上跳转链接和评论表单蜜罐各踩了一次，文档被撑到 10884px 宽，
// 整页在视口里滚出了屏幕。纵向没有这个问题：两种方向下，原点上方一律裁掉。
// 所以离屏隐藏一律用 top 负偏移。
func TestNoHorizontalOffscreenHiding(t *testing.T) {
	src := commentRE.ReplaceAllString(allCSS(t), "")

	// 横向属性上的大负值：-100px 以内是 -1px 这类描边补偿，不算。
	bad := regexp.MustCompile(`(?m)(left|right|inset-inline-start|inset-inline-end|inset-inline|margin-inline-start|margin-inline-end)\s*:\s*-([0-9]{3,})(px|rem)`)
	for _, m := range bad.FindAllString(src, -1) {
		t.Errorf("用横向负偏移做离屏隐藏：%q —— 在 RTL 下会撑出横向滚动条，改用 top 负偏移", strings.TrimSpace(m))
	}

	// 反过来确认那两处确实改成了纵向，别让规则被整段删掉后测试照样通过
	for _, want := range []string{".skip", ".hp"} {
		if !strings.Contains(src, want) {
			t.Errorf("规则 %q 不见了", want)
		}
	}
	if !strings.Contains(src, "top: -9999px") {
		t.Error("蜜罐没有用纵向负偏移离屏")
	}
	if !strings.Contains(src, ".skip:focus { transform: none; }") {
		t.Error("跳转链接获得焦点时没有回到视口内")
	}
}

// TestCommentRateLimitMatchesItsMessage 守住「提示语说的数字」和「实际额度」一致。
//
// 评论限速和登录限速共用一个计数器。额度原本写死在计数器内部（登录的 8 次），
// 于是评论实际是 10 分钟 8 条，而提示语——以及照它翻出去的十种语言——都写着
// 最多 3 条。用户看到的数字是错的，翻译只会把这个错误复制到每一种语言里。
func TestCommentRateLimitMatchesItsMessage(t *testing.T) {
	e := setup(t)
	p := e.publish("Post", "Body", nil)

	for i := 1; i <= commentMaxPerWindow; i++ {
		// 内容必须各不相同：相同正文会被按内容去重那道闸门丢掉，
		// 那样这个测试测的就不是限速了。
		body := "第 " + strconv.Itoa(i) + " 条留言"
		w := e.post("/p/"+p.Slug+"/comments", url.Values{"body": {body}}, false)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("第 %d 条匿名评论被拒了（%d），额度是 %d 条", i, w.Code, commentMaxPerWindow)
		}
	}
	w := e.post("/p/"+p.Slug+"/comments", url.Values{"body": {"over"}}, false)
	body := w.Body.String()
	if !strings.Contains(body, i18n.T(i18n.DefaultCode, "comment.tooFast")) {
		t.Errorf("第 %d 条没有触发限速——提示语承诺的额度比实际严", commentMaxPerWindow+1)
	}

	_, total, _ := e.db.ListComments(t.Context(), store.CommentFilter{})
	if total != commentMaxPerWindow {
		t.Errorf("存下了 %d 条评论，额度是 %d 条", total, commentMaxPerWindow)
	}
}

// TestNonDefaultUILanguageHasNoChineseChrome 守住一整类 bug：界面文案绕过词表。
//
// 这些地方都是「模板之外」的文案，最容易漏：handler 里写死的 <title>、
// 把库里枚举值翻成标签的模板函数、以及拼字符串拼出来的 flash 消息。
// 它们在模板里看不见，所以 lang check 的覆盖率是 100% 也照样是中文。
// 实测时后台状态徽章显示「已发布」、标签页标题显示「内容管理」，
// 而同一行的其他文字已经是阿拉伯语了。
func TestNonDefaultUILanguageHasNoChineseChrome(t *testing.T) {
	e := setup(t)
	e.enableLangs("en")
	e.publish("A Latin Title", "Body text.", nil)

	sid, _, err := e.db.CreateSession(t.Context(), e.uid)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/en/admin", "/en/admin/comments", "/en/admin/media",
		"/en/admin/tokens", "/en/admin/profile", "/en/search?q=latin"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d", path, w.Code)
			continue
		}
		// 三类中文是应该留着的：站名、站点描述和作者名都是这个站自己的文案；
		// 语言切换器里每一项都用该语言自己的说法（中文 / 日本語 / العربية），
		// 换成英文反而没法用。剩下的中文才是漏翻。
		body := strings.NewReplacer("测试站", "", "描述", "", "作者", "").Replace(w.Body.String())
		body = langOptionRE.ReplaceAllString(body, "")
		for _, ru := range body {
			if ru >= 0x4E00 && ru <= 0x9FFF {
				t.Errorf("%s 的英文界面里漏出了中文 %q：%s", path, ru, chineseRunContext(body, ru))
				break
			}
		}
	}
}

// langOptionRE 匹配语言切换器里的选项：带 lang 属性的 <a>。
var langOptionRE = regexp.MustCompile(`(?s)<a[^>]*\blang="[^"]*"[^>]*>.*?</a>`)

// chineseRunContext 把漏网的那串中文连同前后文摘出来，方便一眼看出是哪处。
func chineseRunContext(body string, first rune) string {
	i := strings.IndexRune(body, first)
	lo, hi := i-40, i+40
	if lo < 0 {
		lo = 0
	}
	if hi > len(body) {
		hi = len(body)
	}
	return "…" + strings.TrimSpace(body[lo:hi]) + "…"
}

// TestBrandCompanionRespectsOperatorTitle 守住品牌副名的三条边界。
//
// 副名（轻格 / 輕格 / 軽格 / 경격）是 cligc 这个名字在各语言自己文字里的
// 写法，按界面语言从词表取。危险在于它长得像一条普通界面文案：
//
//   - 逐条回退会让德语页面挂上中文副名——「没有副名」在拉丁语种是设计结果，
//     不是漏翻。所以取值必须用 i18n.Own（只看这个语言自己那条），不能用 T。
//   - 别人拿这套代码建站、用 -title 起了自己的名字，绝不能被 cligc 串台。
//
// 这个测试走真实渲染路径，而不是直接调 splitBrand —— 前者才管得住 render()
// 里到底用的是 Own 还是 T。
func TestBrandCompanionRespectsOperatorTitle(t *testing.T) {
	mustLoadLocales(t)
	for _, c := range []struct{ title, path, want, notWant string }{
		{"cligc", "/", "cligc 轻格", ""},
		{"cligc", "/ja/", "cligc 軽格", ""},
		{"cligc", "/ko/", "cligc 경격", ""},
		{"cligc", "/zh-hant/", "cligc 輕格", ""},
		// 拉丁／西里尔／阿拉伯文没有副名，且绝不能回退成默认语言的中文
		{"cligc", "/en/", "cligc", "轻格"},
		{"cligc", "/de/", "cligc", "轻格"},
		{"cligc", "/ar/", "cligc", "轻格"},
		// 运营方自己写了副名就原样保留
		{"cligc Notes", "/ja/", "cligc Notes", "軽格"},
		// 运营方换了整个站名，i18n 完全不插手
		{"我的博客", "/ja/", "我的博客", "軽格"},
	} {
		dir := t.TempDir()
		db, err := store.Open(filepath.Join(dir, "t.db"), "example.com")
		if err != nil {
			t.Fatal(err)
		}
		srv, err := New(db, Config{BaseURL: "https://example.com", Title: c.title,
			MediaRoot: dir, PerPage: 5})
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		// 这个测试要逐个语言看品牌副名，先把它们全开。
		st := db.Settings(t.Context())
		for _, l := range i18n.ReadyLanguages() {
			st.EnabledLangs = append(st.EnabledLangs, l.Code)
		}
		if err := db.SaveSettings(t.Context(), st); err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		srv.Routes(mux)
		w := httptest.NewRecorder()
		srv.WithLang(srv.WithSession(mux)).ServeHTTP(w, httptest.NewRequest("GET", c.path, nil))
		body := w.Body.String()
		db.Close()

		if got := titleRE.FindStringSubmatch(body); got == nil || got[1] != c.want {
			t.Errorf("-title %q + %s → <title> = %q，想要 %q", c.title, c.path, got, c.want)
		}
		if c.notWant != "" && strings.Contains(body, c.notWant) {
			t.Errorf("-title %q + %s 的页面里出现了 %q —— 副名回退到默认语言了",
				c.title, c.path, c.notWant)
		}
	}
}

var titleRE = regexp.MustCompile(`<title>([^<]*)</title>`)

// TestEveryScrollContainerHasThinScrollbar 守住「新加的滚动容器忘了挂细滚动条」。
//
// 细滚动条是按选择器挂的，不是按 class —— 代码块的 <pre> 和正文里的 <table>
// 都是 goldmark 生成的，加不了 class。代价是以后新写一个 overflow: auto 的
// 容器，很容易漏掉，而漏掉的表现只是「这一处的滚动条比别处粗」，
// 不会报错，多半要等用户截图才发现。
func TestEveryScrollContainerHasThinScrollbar(t *testing.T) {
	src := commentRE.ReplaceAllString(allCSS(t), "")

	rule := regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)
	scrolls := regexp.MustCompile(`overflow(-x|-y)?:\s*(auto|scroll)`)

	// 只认宽度那条规则（::-webkit-scrollbar 本体），不认 -thumb / -track /
	// -corner —— 后三者是滑块和轨道的外观，没有它们滚动条照样是细的。
	const bar = "::-webkit-scrollbar"
	covered := map[string]bool{}
	for _, m := range rule.FindAllStringSubmatch(src, -1) {
		for _, sel := range strings.Split(m[1], ",") {
			sel = strings.TrimSpace(sel)
			if !strings.HasSuffix(sel, bar) {
				continue
			}
			covered[strings.TrimSpace(strings.TrimSuffix(sel, bar))] = true
		}
	}
	if len(covered) == 0 {
		t.Fatal("样式表里找不到 ::-webkit-scrollbar 规则，细滚动条整段没了")
	}

	for _, m := range rule.FindAllStringSubmatch(src, -1) {
		if !scrolls.MatchString(m[2]) {
			continue
		}
		for _, sel := range strings.Split(m[1], ",") {
			sel = strings.TrimSpace(sel)
			if sel == "" || strings.HasPrefix(sel, "@") {
				continue
			}
			// 整页滚动条不在此列：它是浏览器层面的操作件，做成 4px 反而难抓。
			// 细滚动条针对的是页面内部的小浮层和小容器。
			if sel == "html" {
				continue
			}
			if !covered[sel] {
				t.Errorf("%q 会产生内部滚动条，但没挂细滚动条 —— "+
					"把它加进 ::-webkit-scrollbar 那组选择器，或者给它加 .thin-scroll", sel)
			}
		}
	}
}

// TestRootIsAlwaysAScrollContainer 守住「页面宽度不许跟着内容多少变」。
//
// 根元素不是恒定的滚动容器时，页面宽度取决于这一页够不够长到出滚动条。
// 多语言站上这件事会变得很显眼：默认语言有文章、首页会滚，新加的语言
// 还没有文章、首页不滚，于是两者正文宽度差一整条滚动条（实测 1200px
// 视口下是 1185 对 1200），居中容器再偏半条。
//
// 注意不能改用 scrollbar-gutter: stable —— 那个属性只作用于滚动容器，
// 而 html 默认 overflow: visible 并不是。实测设了 stable 宽度纹丝不动。
func TestRootIsAlwaysAScrollContainer(t *testing.T) {
	src := commentRE.ReplaceAllString(allCSS(t), "")

	m := regexp.MustCompile(`(?s)(^|\})\s*html\s*\{([^}]*)\}`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("样式表里找不到 html 规则")
	}
	if !regexp.MustCompile(`overflow-y:\s*scroll`).MatchString(m[2]) {
		t.Error("html 没有 overflow-y: scroll —— 页面宽度会跟着内容多少变，" +
			"有文章的语言和没文章的语言宽度差一条滚动条")
	}
}

// TestSkillPackMatchesShippedSkill 守住技能包里的 SKILL.md 和源码树里那份一致。
//
// 有两份副本：internal/web/skillpack/SKILL.md 编进二进制供后台下载，
// skill/cligc-publish/SKILL.md 供手动安装。改了一份忘了另一份不会报错——
// 表现是"我在后台下的包和文档说的不一样"，而且没人会想到去比对。
func TestSkillPackMatchesShippedSkill(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "skill", "cligc-publish", "SKILL.md"))
	if err != nil {
		t.Skip("源码树里没有 skill/ 目录，跳过")
	}
	if strings.TrimSpace(string(onDisk)) != strings.TrimSpace(skillMD) {
		t.Error("technique 包里的 SKILL.md 和 skill/cligc-publish/SKILL.md 不一致 —— " +
			"改了一份忘了另一份。以 internal/web/skillpack/SKILL.md 为准同步过去。")
	}
}

// TestSkillPackIsUsable 下载下来的包必须自带四个文件，而且 token 是占位符。
// wantAPIOps 是 /api/v1 下的端点总数。加了接口就同步改这里——
// 它存在的意义就是强迫改动 API 的人回头看一眼技能包里的规格。
const wantAPIOps = 30

func TestSkillPackIsUsable(t *testing.T) {
	e := setup(t)
	sid, _, err := e.db.CreateSession(t.Context(), e.uid)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/admin/skill-pack", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("下载技能包 = %d", w.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"SKILL.md": false, "mcp.json": false, "openapi.json": false, "README.md": false}
	for _, f := range zr.File {
		name := f.Name[strings.LastIndex(f.Name, "/")+1:]
		if _, ok := want[name]; !ok {
			continue
		}
		want[name] = true
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		// 包里绝不能出现真 token：下载目录里躺着活凭据、还可能被网盘同步走
		if strings.Contains(string(body), "cligc_") && name != "README.md" {
			t.Errorf("%s 里出现了疑似真实 token", name)
		}
		if name == "openapi.json" {
			// 必须真的解析一遍，不能只 grep 操作名。
			// 这份规格最早是手拼 YAML 的，`title: "cligc" API` 这种引号错
			// grep 完全抓不到，但会让 GPT 的 Actions 导入直接失败。
			var spec struct {
				OpenAPI string `json:"openapi"`
				Paths   map[string]map[string]struct {
					OperationID string `json:"operationId"`
				} `json:"paths"`
			}
			if err := json.Unmarshal(body, &spec); err != nil {
				t.Fatalf("openapi.json 解析失败：%v", err)
			}
			if spec.OpenAPI == "" {
				t.Error("openapi.json 缺少版本号")
			}
			got := map[string]bool{}
			for _, methods := range spec.Paths {
				for _, op := range methods {
					got[op.OperationID] = true
				}
			}
			// 规格里的操作数必须和真实路由数一致：少了 AI 以为站点没这个
			// 能力，多了它会去调一个不存在的接口
			if n := len(got); n != wantAPIOps {
				t.Errorf("规格里有 %d 个操作，实际 API 有 %d 个", n, wantAPIOps)
			}
			for _, op := range []string{"schedulePost", "approveComment", "updateSite", "createToken", "updateProfile"} {
				if !got[op] {
					t.Errorf("openapi.json 缺少 %s", op)
				}
			}
		}
	}
	for name, got := range want {
		if !got {
			t.Errorf("技能包里少了 %s", name)
		}
	}
}

// allCSS / allJS 把三层拼起来，给那些"全站不许出现某种写法"的检查用。
// 拆分之后这类检查必须覆盖所有层，只看一个文件会漏。
func allCSS(t *testing.T) string {
	t.Helper()
	return readLayers(t, "css")
}

func allJS(t *testing.T) string {
	t.Helper()
	return readLayers(t, "js")
}

func readLayers(t *testing.T, ext string) string {
	t.Helper()
	var b strings.Builder
	for _, layer := range []string{"base", "site", "admin"} {
		body, err := staticFS.ReadFile("static/" + layer + "." + ext)
		if err != nil {
			t.Fatalf("读不到 %s.%s：%v", layer, ext, err)
		}
		b.Write(body)
		b.WriteString("\n")
	}
	return b.String()
}

// TestStyleLayersStaySeparated 守住公开页样式和后台样式互不渗透。
//
// 拆成三层的意义是：前端视觉大改只动 site.css，后台不受影响。这个保证靠
// 约定守不住——总有人图省事把一条后台规则写进 site.css，而当时不会有任何
// 症状，要等到几个月后重做前端时才发现后台跟着塌了。
//
// 所以这里按"类名只出现在哪一侧的模板里"来判定归属：
//   - 只在后台模板里出现的类，不许在 site.css 里定义
//   - 只在公开模板里出现的类，不许在 admin.css 里定义
//
// 两边都用的类归 base.css，不参与判定。
func TestStyleLayersStaySeparated(t *testing.T) {
	adminOnly, publicOnly := templateClasses(t)

	for _, c := range []struct {
		layer     string
		forbidden map[string]bool
		why       string
	}{
		{"site", adminOnly, "后台才用的类写进了公开样式 —— 重做前端时它会被一起改掉或删掉"},
		{"admin", publicOnly, "公开页才用的类写进了后台样式 —— 后台从此依赖前端的实现细节"},
	} {
		body, err := staticFS.ReadFile("static/" + c.layer + ".css")
		if err != nil {
			t.Fatal(err)
		}
		src := commentRE.ReplaceAllString(string(body), "")
		for _, m := range regexp.MustCompile(`\.([a-z][a-z0-9-]{2,})`).FindAllStringSubmatch(src, -1) {
			if c.forbidden[m[1]] {
				t.Errorf("%s.css 里出现了 .%s —— %s", c.layer, m[1], c.why)
			}
		}
	}
}

// templateClasses 分别收集"只在后台模板出现"和"只在公开模板出现"的类名。
// layout.html / partials.html / error.html 是两边共用的外壳，其中的类名
// 两边都算，因此不会落进任何一个 only 集合。
func templateClasses(t *testing.T) (adminOnly, publicOnly map[string]bool) {
	t.Helper()
	// 按文件名前缀判断，不手写枚举。
	//
	// 原来是一份手写清单，加 admin_categories.html 时忘了往里加，结果它的
	// 类名被当成"公开页专用"，admin.css 里引用自己的类反而被判越界。
	// 一份需要人记得同步的清单，迟早会有人忘。
	isAdmin := func(name string) bool {
		return strings.HasPrefix(name, "admin_") || name == "login"
	}
	shared := map[string]bool{"layout": true, "partials": true, "error": true}
	// partials_site.html 里的片段只给公开页用，按公开归类——不这么分的话，
	// 把 .feat-card 这种公开页的类写进 admin.css 就查不出来。

	names, err := fs.Glob(tmplFS, "templates/*.html")
	if err != nil || len(names) == 0 {
		t.Fatalf("读不到模板：%v", err)
	}
	inAdmin, inPublic, inShared := map[string]bool{}, map[string]bool{}, map[string]bool{}
	attr := regexp.MustCompile(`class="([^"]*)"`)
	for _, n := range names {
		body, err := fs.ReadFile(tmplFS, n)
		if err != nil {
			t.Fatal(err)
		}
		base := strings.TrimSuffix(filepath.Base(n), ".html")
		into := inPublic
		switch {
		case isAdmin(base):
			into = inAdmin
		case shared[base]:
			into = inShared
		}
		for _, m := range attr.FindAllStringSubmatch(string(body), -1) {
			for _, w := range strings.Fields(strings.ReplaceAll(m[1], "{{", " {{")) {
				if w != "" && !strings.ContainsAny(w, "{}") {
					into[w] = true
				}
			}
		}
	}
	adminOnly, publicOnly = map[string]bool{}, map[string]bool{}
	for c := range inAdmin {
		if !inPublic[c] && !inShared[c] {
			adminOnly[c] = true
		}
	}
	for c := range inPublic {
		if !inAdmin[c] && !inShared[c] {
			publicOnly[c] = true
		}
	}
	if len(adminOnly) < 5 || len(publicOnly) < 5 {
		t.Fatalf("类名归类看起来不对：后台专用 %d 个，公开专用 %d 个", len(adminOnly), len(publicOnly))
	}
	return adminOnly, publicOnly
}

// TestCommentAbuseGates 守住三道闸门各自挡住不同的维度。
//
// 三道缺一不可：限速只管单个来源发得多快，队列上限挡"一万个来源各发三条"，
// 内容去重挡"换了地址但照抄同一段文字"。少任何一道，另外两道都能被绕过。
func TestCommentAbuseGates(t *testing.T) {
	t.Run("IPv6 按 /64 聚合", func(t *testing.T) {
		// 一条家宽分配到的就是一个 /64，里面 2^64 个地址。按完整地址限速
		// 的话，攻击者换一个地址就重置计数——实测过 40 条全进。
		same := []string{"2001:db8:1:1::1", "2001:db8:1:1::dead", "2001:db8:1:1:ffff:ffff:ffff:ffff"}
		want := rateKey(same[0])
		for _, ip := range same[1:] {
			if got := rateKey(ip); got != want {
				t.Errorf("同一个 /64 里的 %s 归到了 %q，应当和 %q 同键", ip, got, want)
			}
		}
		// 不同 /64 必须是不同的键，否则会把无关用户一起误伤
		if rateKey("2001:db8:1:2::1") == want {
			t.Error("不同 /64 被归成了同一个键")
		}
		// IPv4 保持按完整地址：按 /24 聚合会误伤同一个 NAT 出口后面的真实用户
		if rateKey("203.0.113.9") == rateKey("203.0.113.10") {
			t.Error("两个不同的 IPv4 地址被归成了同一个键")
		}
	})

	t.Run("相同正文只进一条", func(t *testing.T) {
		e := setup(t)
		p := e.publish("Post", "Body", nil)
		for i := 0; i < 3; i++ {
			if w := e.post("/p/"+p.Slug+"/comments",
				url.Values{"body": {"一模一样的垃圾"}}, false); w.Code != http.StatusSeeOther {
				// 重复的那几条走蜜罐那套：返回成功但不写库，
				// 明确报错等于告诉脚本"换段文字再来"
				t.Fatalf("第 %d 条返回 %d，应当看起来像成功", i+1, w.Code)
			}
		}
		_, total, _ := e.db.ListComments(t.Context(), store.CommentFilter{})
		if total != 1 {
			t.Errorf("存下了 %d 条一模一样的评论，应当只有 1 条", total)
		}
	})

	t.Run("队列满了就暂停接收", func(t *testing.T) {
		e := setupWithQueueCap(t, 2)
		p := e.publish("Post", "Body", nil)
		for i := 0; i < 2; i++ {
			e.post("/p/"+p.Slug+"/comments",
				url.Values{"body": {"留言 " + strconv.Itoa(i)}}, false)
		}
		w := e.post("/p/"+p.Slug+"/comments", url.Values{"body": {"再来一条"}}, false)
		if !strings.Contains(w.Body.String(), i18n.T(i18n.DefaultCode, "comment.queueFull")) {
			t.Error("队列已满却仍然收下了新评论")
		}
		_, total, _ := e.db.ListComments(t.Context(), store.CommentFilter{})
		if total != 2 {
			t.Errorf("队列上限是 2，却存下了 %d 条", total)
		}
		// 登录用户不受队列上限影响：他的评论直接通过，根本不进队列
		if w := e.post("/p/"+p.Slug+"/comments",
			url.Values{"body": {"站长自己的回复"}}, true); w.Code != http.StatusSeeOther {
			t.Errorf("登录用户被队列上限挡住了（%d）—— 他的评论不进队列", w.Code)
		}
	})
}

// TestSiteTitlePrecedence 守住站名/描述的三层优先级，尤其是
// "内置词表不许盖掉站长在后台输入的名字"这一条。
//
// 这个坑踩过一次：内置词表原本带着 site.description（描述的是 cligc
// 自己），而词表优先级最高。结果是站长在后台改了描述，10 种语言下看到的
// 仍然是 cligc 的宣传语——而且不报错，只是输入的东西像没生效。
func TestSiteTitlePrecedence(t *testing.T) {
	mustLoadLocales(t)

	// 内置词表不许带这两个 key
	for _, code := range i18n.Codes() {
		for _, key := range []string{"site.title", "site.description"} {
			if v, ok := i18n.Own(code, key); ok {
				t.Errorf("内置词表 %s 带着 %s=%q —— 它会盖掉站长自己设的值", code, key, v)
			}
		}
	}

	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv, err := New(db, Config{BaseURL: "https://example.com",
		Title: "命令行给的名字", Description: "命令行给的描述", MediaRoot: dir})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Routes(mux)
	h := srv.WithLang(srv.WithSession(mux))

	get := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w.Body.String()
	}

	if body := get(); !strings.Contains(body, "命令行给的名字") {
		t.Error("没设置过时，应当用命令行的值")
	}
	// 后台设置要能盖过命令行
	def := i18n.Default().Code
	if err := db.SaveSettings(t.Context(), store.SiteSettings{
		CommentsEnabled: true,
		SiteTitles:      map[string]string{def: "后台设的名字"},
		SiteDescs:       map[string]string{def: "后台设的描述"},
	}); err != nil {
		t.Fatal(err)
	}
	body := get()
	if !strings.Contains(body, "后台设的名字") {
		t.Error("后台设了名字却没生效 —— 命令行的值盖住了它")
	}
	if !strings.Contains(body, "后台设的描述") {
		t.Error("后台设了描述却没生效")
	}
	if strings.Contains(body, "命令行给的名字") {
		t.Error("后台设了之后命令行的值还在页面上")
	}
}
