package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"cligc.com/internal/store"
)

// authGet 带登录会话取一个页面。
func (e *env) authGet(path string) (int, string) {
	e.t.Helper()
	sid, _, err := e.db.CreateSession(e.t.Context(), e.uid)
	if err != nil {
		e.t.Fatal(err)
	}
	r := httptest.NewRequest("GET", path, nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// TestSiteSettingsFormControlsLanguagesAndCopy 走一遍设置页的表单：
// 勾语言、按语言填站名，两件事都要在对外页面上生效。
func TestSiteSettingsFormControlsLanguagesAndCopy(t *testing.T) {
	e := setup(t)

	// 这一页拆成了好几张表单，一次请求只动一块。
	for _, form := range []url.Values{
		{"section": {"langs"}, "langs": {"en"}},
		{"section": {"copy"},
			"site_title:zh-Hans": {"我的博客"},
			"site_title:en":      {"My Journal"},
			"site_desc:en":       {"Notes on things."}},
	} {
		if w := e.post("/admin/site", form, true); w.Code != http.StatusSeeOther {
			t.Fatalf("保存 %s = %d，想要 303", form.Get("section"), w.Code)
		}
	}

	if _, body := e.get("/"); !strings.Contains(body, "我的博客") {
		t.Error("默认语言首页没用上后台填的站名")
	}
	code, body := e.get("/en/")
	if code != http.StatusOK {
		t.Fatalf("/en/ = %d，勾了就该能访问", code)
	}
	if !strings.Contains(body, "My Journal") {
		t.Error("英文首页没用上英文站名")
	}
	if strings.Contains(body, "我的博客") {
		t.Error("英文页上出现了中文站名——按语言存的两份串了")
	}
	if !strings.Contains(body, "Notes on things.") {
		t.Error("英文首页没用上英文描述")
	}

	// 没勾的语言整条路径都不该存在。留一个内容为空的列表页给爬虫，
	// 比不提供这个语言更糟。
	if code, _ := e.get("/de/"); code != http.StatusNotFound {
		t.Errorf("/de/ = %d，没勾的语言想要 404", code)
	}
	// 但后台不受这条限制：管理界面用哪种语言是站长自己的偏好，
	// 和站点对外提供哪些语言是两回事。
	if code, _ := e.authGet("/de/admin"); code != http.StatusOK {
		t.Errorf("/de/admin = %d，后台不该被对外语言开关挡住", code)
	}
}

// TestSiteSettingsFormKeepsLanguagesItCannotExpress 表单只渲染达标的语言，
// 保存时不能把它表达不了的那些无声关掉。
//
// 词表改动让一个语言暂时掉到覆盖率门槛以下，它就不再出现在勾选框里。
// 这时候去改个 GA4 ID 顺手保存，不该把那个语言一起关了。
func TestSiteSettingsFormKeepsLanguagesItCannotExpress(t *testing.T) {
	e := setup(t)
	e.enableLangs("en", "xx-Custom")

	if w := e.post("/admin/site", url.Values{
		"section": {"langs"}, "langs": {"en"},
	}, true); w.Code != http.StatusSeeOther {
		t.Fatalf("保存设置 = %d，想要 303", w.Code)
	}

	got := e.db.Settings(t.Context()).EnabledLangs
	if !slices.Contains(got, "xx-Custom") {
		t.Errorf("EnabledLangs = %v，表单渲染不出的语言被无声关掉了", got)
	}
	if !slices.Contains(got, "en") {
		t.Errorf("EnabledLangs = %v，缺了表单里勾着的 en", got)
	}
}

// TestHomePagesAreAlternatesOfEachOther 各语言首页要互相引用 hreflang。
//
// 首页是站里除文章外唯一进索引的页面，也是品牌词排的那一页。文章页靠
// 译文分组连起来，首页没有那个东西——它的对应关系来自"站长开了哪些语言"。
func TestHomePagesAreAlternatesOfEachOther(t *testing.T) {
	e := setup(t)

	// 只开一种语言时不该发：没有"其它版本"这回事。
	if _, body := e.get("/"); strings.Contains(body, `hreflang=`) {
		t.Error("单语言站的首页不该有 hreflang")
	}

	e.enableLangs("en")
	for _, tc := range []struct{ path, self string }{
		{"/", "https://example.com/"},
		{"/en/", "https://example.com/en/"},
	} {
		_, body := e.get(tc.path)
		for _, want := range []string{
			`<link rel="alternate" hreflang="zh-Hans" href="https://example.com/">`,
			`<link rel="alternate" hreflang="en" href="https://example.com/en/">`,
			`<link rel="alternate" hreflang="x-default" href="https://example.com/">`,
			`<link rel="canonical" href="` + tc.self + `">`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 少了 %s", tc.path, want)
			}
		}
	}

	// 没开的语言不能出现在里面——那等于给一个 404 发 hreflang。
	if _, body := e.get("/"); strings.Contains(body, `hreflang="de"`) {
		t.Error("首页给没启用的语言发了 hreflang")
	}
}

// TestSiteSettingsPageShowsPerLanguageCounts 语言那一栏要显示每个语种
// 已发布多少篇——勾一个语言等于对搜索引擎宣称"这里有这个语种"，
// 得让人在勾之前看见那边到底有没有东西。
func TestSiteSettingsPageShowsPerLanguageCounts(t *testing.T) {
	e := setup(t)
	e.publish("一篇", "正文", nil)

	code, body := e.authGet("/admin/site")
	if code != http.StatusOK {
		t.Fatalf("/admin/site = %d", code)
	}
	if !strings.Contains(body, `name="site_title:en"`) {
		t.Error("设置页没给英文留站名输入框")
	}
	if !strings.Contains(body, `value="zh-Hans"`) {
		t.Error("设置页没列出默认语言的勾选项")
	}
	if !strings.Contains(body, "共 1 篇") {
		t.Error("设置页没显示默认语言的文章数")
	}
	if !strings.Contains(body, "还没有内容") {
		t.Error("设置页没提示空语种还没有内容")
	}
}

// TestSiteSectionsSaveIndependently 站点设置拆成了好几张表单，
// 存一块不能把别的块清掉。
//
// 具体的坏结果：统计那张表单里没有 comments 字段，照旧
// r.FormValue("comments") 得到空串——于是改一次 GA4 ID 就把评论关了，
// 而且不报错。
func TestSiteSectionsSaveIndependently(t *testing.T) {
	e := setup(t)

	// 先把每一块都填上
	for _, f := range []url.Values{
		{"section": {"langs"}, "langs": {"en"}},
		{"section": {"copy"}, "site_title:zh-Hans": {"我的博客"}},
		{"section": {"seo"}, "google_verify": {"gsc-token"}, "bing_verify": {"bing-token"}},
		{"section": {"comments"}, "comments": {"1"}},
		{"section": {"analytics"}, "ga4_id": {"G-FIRST"}},
	} {
		if w := e.post("/admin/site", f, true); w.Code != http.StatusSeeOther {
			t.Fatalf("保存 %s = %d", f.Get("section"), w.Code)
		}
	}

	// 只改统计
	if w := e.post("/admin/site", url.Values{
		"section": {"analytics"}, "ga4_id": {"G-SECOND"},
	}, true); w.Code != http.StatusSeeOther {
		t.Fatal("保存统计失败")
	}

	st := e.db.Settings(t.Context())
	if st.GA4ID != "G-SECOND" {
		t.Errorf("GA4 = %q，没改上", st.GA4ID)
	}
	if !st.CommentsEnabled {
		t.Error("改一次 GA4 把评论关了")
	}
	if st.GoogleVerify != "gsc-token" || st.BingVerify != "bing-token" {
		t.Errorf("改一次 GA4 把验证码清了：%q / %q", st.GoogleVerify, st.BingVerify)
	}
	if st.SiteTitles["zh-Hans"] != "我的博客" {
		t.Errorf("改一次 GA4 把站名清了：%q", st.SiteTitles["zh-Hans"])
	}
	if !slices.Contains(st.EnabledLangs, "en") {
		t.Errorf("改一次 GA4 把语言关了：%v", st.EnabledLangs)
	}

	// 没有 section 的请求要拒掉，而不是当成"全都清空"
	if w := e.post("/admin/site", url.Values{"ga4_id": {"G-X"}}, true); w.Code != http.StatusBadRequest {
		t.Errorf("没带 section 的提交 = %d，想要 400", w.Code)
	}
}

// TestCategoryRowSaveKeepsOrder 表格里没有"排序"那一列了，
// 存一行不能把它挪到最前面。
func TestCategoryRowSaveKeepsOrder(t *testing.T) {
	e := setup(t)
	three := 3
	c, err := e.db.CreateCategory(t.Context(), store.CategoryInput{
		Slug: "essays", Sort: &three, DefaultLang: "zh-Hans",
		Names: map[string]string{"zh-Hans": "随笔"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 行表单带的就是这几个字段——没有 sort
	if w := e.post("/admin/categories", url.Values{
		"id": {strconv.FormatInt(c.ID, 10)}, "slug": {"essays"},
		"name:zh-Hans": {"随笔集"},
	}, true); w.Code != http.StatusSeeOther {
		t.Fatalf("保存分类行 = %d", w.Code)
	}

	got, err := e.db.CategoryBySlug(t.Context(), "essays", "zh-Hans")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "随笔集" {
		t.Errorf("名字没改上：%q", got.Name)
	}
	if got.Sort != 3 {
		t.Errorf("排序被改成了 %d，表单里没有这个字段就不该动它", got.Sort)
	}
}

// TestSettingsPageAbsorbedTabs 账号和 API Token 并进了设置页，
// 老地址保留为跳转——它们可能在书签里。
func TestSettingsPageAbsorbedTabs(t *testing.T) {
	e := setup(t)

	for _, old := range []string{"/admin/profile", "/admin/tokens"} {
		if code, _ := e.authGet(old); code != http.StatusFound {
			t.Errorf("%s = %d，想要 302 跳到 /admin/site", old, code)
		}
	}

	_, body := e.authGet("/admin/site")
	for _, want := range []string{
		`name="section" value="langs"`, // 语言
		`name="ga4_id"`,                // 统计
		`action="/admin/profile"`,      // 资料
		`action="/admin/tokens"`,       // 新建 token 的表单
	} {
		if !strings.Contains(body, want) {
			t.Errorf("设置页上少了 %s", want)
		}
	}
	// 顶部导航里不该再有这两个标签
	for _, gone := range []string{`href="/admin/profile"`, `href="/admin/tokens"`} {
		if strings.Contains(body, gone) {
			t.Errorf("导航里还留着 %s", gone)
		}
	}

	// 建一个 token 要回到设置页，并且把明文带回来显示一次
	w := e.post("/admin/tokens", url.Values{
		"name": {"t1"}, "days": {"7"}, "scopes": {"posts:read"},
	}, true)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("创建 token = %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/admin/site?") || !strings.Contains(loc, "token=") {
		t.Errorf("创建后跳到了 %q，想要 /admin/site?token=…", loc)
	}
}
