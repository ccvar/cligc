package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
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

	w := e.post("/admin/site", url.Values{
		"langs":              {"en"},
		"site_title:zh-Hans": {"我的博客"},
		"site_title:en":      {"My Journal"},
		"site_desc:en":       {"Notes on things."},
		"comments":           {"1"},
	}, true)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("保存设置 = %d，想要 303", w.Code)
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
		"langs":  {"en"},
		"ga4_id": {"G-ABC"},
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
