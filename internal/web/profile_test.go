package web

import (
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"cligc.com/internal/store"
)

// TestProfileSaveKeepsHiddenFields 单人站上表单里没有"主页地址"和"简介"
// 这两个框，保存显示名不能把它们连带抹掉。
//
// 这是一条无声的数据丢失：slug 会按新名字重算，旧的 /u/xxx 直接 404，
// 而页面上没有任何地方提示发生了这件事。
func TestProfileSaveKeepsHiddenFields(t *testing.T) {
	e := setup(t)

	if _, err := e.db.UpdateUser(t.Context(), e.uid, "老名字", "一句简介", "my-page"); err != nil {
		t.Fatal(err)
	}

	// 只提交显示名，模拟单人站上那个表单实际会发出的内容
	if w := e.post("/admin/profile", url.Values{"name": {"新名字"}}, true); w.Code != http.StatusSeeOther {
		t.Fatalf("保存资料 = %d，想要 303", w.Code)
	}

	u, err := e.db.UserByID(t.Context(), e.uid)
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "新名字" {
		t.Errorf("显示名 = %q，没改上", u.Name)
	}
	if u.Bio != "一句简介" {
		t.Errorf("简介 = %q，表单里没有这个字段就不该动它", u.Bio)
	}
	if u.Slug != "my-page" {
		t.Errorf("slug = %q，想要 my-page——改一次显示名不该让旧地址 404", u.Slug)
	}

	// 资料并进了站点设置页，老地址跳过去
	if code, _ := e.authGet("/admin/profile"); code != http.StatusFound {
		t.Errorf("/admin/profile = %d，想要 302 跳到 /admin/site", code)
	}

	// 单人站上这两个框根本不该渲染出来
	_, body := e.authGet("/admin/site")
	if strings.Contains(body, `name="slug"`) {
		t.Error("单人站的资料页上出现了「主页地址」——那一页是 noindex 且没人链过去")
	}
	if strings.Contains(body, `name="bio"`) {
		t.Error("单人站的资料页上出现了「简介」——它唯一露面的地方是 /u/{slug}")
	}
	if !strings.Contains(body, `name="name"`) {
		t.Error("显示名不见了——它出现在每篇文章的署名行上")
	}

	// 多作者时它们才出现，提交了就该生效，包括清空简介
	if _, err := e.db.CreateUser(t.Context(), "b@c.com", "另一位", "password123", "author"); err != nil {
		t.Fatal(err)
	}
	_, body = e.authGet("/admin/site")
	if !strings.Contains(body, `name="slug"`) || !strings.Contains(body, `name="bio"`) {
		t.Error("多作者站上这两个框该出现")
	}
	if w := e.post("/admin/profile", url.Values{
		"name": {"新名字"}, "slug": {"other-page"}, "bio": {""},
	}, true); w.Code != http.StatusSeeOther {
		t.Fatal("带上字段保存失败")
	}
	u, _ = e.db.UserByID(t.Context(), e.uid)
	if u.Slug != "other-page" || u.Bio != "" {
		t.Errorf("显式提交的值没生效：slug=%q bio=%q", u.Slug, u.Bio)
	}
}

// TestAdminListLanguageFilter 后台列表的语言筛选。
//
// 筛选器列的是**选了能筛出东西**的那些值，不是站点启用了哪些语言：
// 只有中文文章的站，下拉里摆十种语言，选哪个都是空列表。
func TestAdminListLanguageFilter(t *testing.T) {
	e := setup(t)

	// 只有一种语言时不该出现这个下拉
	e.publish("中文的一篇", "正文", nil)
	if _, body := e.authGet("/admin"); strings.Contains(body, `name="lang"`) {
		t.Error("只有一种语言的站上出现了语言筛选")
	}

	a := store.Actor{UserID: e.uid, Kind: "web"}
	if _, err := e.db.CreatePost(t.Context(), a, store.CreatePostInput{
		Title: "An English One", BodyMD: "Body.", Lang: "en",
	}); err != nil {
		t.Fatal(err)
	}

	_, body := e.authGet("/admin")
	if !strings.Contains(body, `name="lang"`) {
		t.Fatal("有两种语言的文章了，筛选器还没出现")
	}
	// 选项要带 lang 属性：那是 HTML 自己表达"这段是那个语言的文字"的方式，
	// 读屏软件靠它切换发音。
	if !strings.Contains(body, `value="en" lang="en"`) {
		t.Error("语言选项没标 lang 属性")
	}
	// 库里没有的语言不该出现在下拉里
	if strings.Contains(body, `value="ja"`) {
		t.Error("下拉里列了一个一篇文章都没有的语言")
	}

	_, zh := e.authGet("/admin?lang=zh-Hans")
	if !strings.Contains(zh, "中文的一篇") || strings.Contains(zh, "An English One") {
		t.Error("按 zh-Hans 筛选没把英文那篇滤掉")
	}
	_, en := e.authGet("/admin?lang=en")
	if !strings.Contains(en, "An English One") || strings.Contains(en, "中文的一篇") {
		t.Error("按 en 筛选没把中文那篇滤掉")
	}
	// 搜索和语言筛选要能叠加
	_, both := e.authGet("/admin?q=English&lang=zh-Hans")
	if strings.Contains(both, "An English One") {
		t.Error("搜索时语言筛选没生效")
	}
}

// TestSearchIsLanguageScoped 站内检索也得按语言分开。
//
// 列表页早就是按语言过滤的，检索这一条却漏了——SearchFilter 上有 Lang
// 字段、handleSearch 也一直在传，但 Search 的 SQL 里从来没用过它。
// 表现是：在 /en/ 搜一个词，中文文章混在结果里，点进去跳到中文地址。
func TestSearchIsLanguageScoped(t *testing.T) {
	e := setup(t)
	e.enableLangs("en")

	e.publish("服务端渲染这件事", "这里写的是服务端渲染 rendering 的取舍。", nil)
	a := store.Actor{UserID: e.uid, Kind: "web"}
	p, err := e.db.CreatePost(t.Context(), a, store.CreatePostInput{
		Title: "Server rendering", BodyMD: "About server rendering tradeoffs.", Lang: "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.PublishPost(t.Context(), a, p.ID, 0); err != nil {
		t.Fatal(err)
	}

	// 命中的词会被 <mark> 包起来，所以比对前先把标签去掉——
	// 直接 Contains("Server rendering") 会被 "Server <mark>rendering</mark>" 骗过去。
	strip := func(s string) string { return tagRE.ReplaceAllString(s, "") }

	_, en := e.get("/en/search?q=rendering")
	if !strings.Contains(strip(en), "Server rendering") {
		t.Error("英文站搜不到英文文章")
	}
	if strings.Contains(en, "服务端渲染这件事") {
		t.Error("英文站的检索结果里混进了中文文章")
	}

	_, zh := e.get("/search?q=rendering")
	if !strings.Contains(zh, "服务端渲染这件事") {
		t.Error("中文站搜不到中文文章")
	}
	if strings.Contains(strip(zh), "Server rendering") {
		t.Error("中文站的检索结果里混进了英文文章")
	}
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

// TestCategoryNamesFollowLanguage 板块名要跟着语言走，侧栏不许列出
// 当前语言下一篇都没有的板块。
//
// 两件事分别对应两种坏结果：中文板块名出现在英文站上，读者看不懂；
// 侧栏写着"随笔 9"、点进去一篇都没有，读者以为站坏了——而那个 9 是
// 全站口径，英文那边其实是 0。
func TestCategoryNamesFollowLanguage(t *testing.T) {
	e := setup(t)
	e.enableLangs("en")

	cat, err := e.db.CreateCategory(t.Context(), store.CategoryInput{
		Slug: "essays", DefaultLang: "zh-Hans",
		Names: map[string]string{"zh-Hans": "随笔", "en": "Essays"},
		Descs: map[string]string{"zh-Hans": "随笔与杂记", "en": "Short pieces."},
	})
	if err != nil {
		t.Fatal(err)
	}

	a := store.Actor{UserID: e.uid, Kind: "web"}
	zh, err := e.db.CreatePost(t.Context(), a, store.CreatePostInput{
		Title: "一篇随笔", BodyMD: "正文", CategorySlug: cat.Slug})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.PublishPost(t.Context(), a, zh.ID, 0); err != nil {
		t.Fatal(err)
	}

	// 这时候英文站还没有属于这个板块的文章
	_, enHome := e.get("/en/")
	if strings.Contains(enHome, "随笔") {
		t.Error("英文首页的侧栏里出现了中文板块名")
	}
	if strings.Contains(enHome, "Essays") {
		t.Error("英文侧栏列了一个英文下一篇都没有的板块——点进去是空的")
	}
	_, zhHome := e.get("/")
	if !strings.Contains(zhHome, "随笔") {
		t.Error("中文首页的侧栏里没有这个板块")
	}

	// 加一篇英文的进去，英文侧栏才该出现它，而且用英文名
	en, err := e.db.CreatePost(t.Context(), a, store.CreatePostInput{
		Title: "An Essay", BodyMD: "Body.", Lang: "en", CategorySlug: cat.Slug})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.PublishPost(t.Context(), a, en.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, enHome = e.get("/en/")
	if !strings.Contains(enHome, "Essays") {
		t.Error("英文侧栏还是没有这个板块")
	}
	if strings.Contains(enHome, "随笔") {
		t.Error("英文侧栏用的还是中文名")
	}

	// 板块页本身也要用当前语言的名字和描述
	code, enCat := e.get("/en/c/essays")
	if code != http.StatusOK {
		t.Fatalf("/en/c/essays = %d", code)
	}
	if !strings.Contains(enCat, "Essays") || strings.Contains(enCat, "随笔") {
		t.Error("英文板块页上的名字不对")
	}
	if !strings.Contains(enCat, "Short pieces.") {
		t.Error("英文板块页没用上英文描述")
	}

	// 没填译名的语言退回默认语言那一份，而不是显示空标题
	if err := e.db.UpdateCategory(t.Context(), cat.ID, store.CategoryInput{
		Slug: "essays", DefaultLang: "zh-Hans",
		Names: map[string]string{"zh-Hans": "随笔", "en": ""},
		Descs: map[string]string{"zh-Hans": "随笔与杂记"},
	}); err != nil {
		t.Fatal(err)
	}
	_, enCat = e.get("/en/c/essays")
	if !strings.Contains(enCat, "随笔") {
		t.Error("英文译名清空后没退回默认语言那一份，标题成了空的")
	}
}

// TestNoFormInsideTableRow 表格行里不许出现 <form>。
//
// HTML 的表格解析规则不允许 <tr> 里直接放 <form>：外层已经有一张表单
// 把"表单指针"占住时，这些 <form> 开始标签会被**整个忽略**。结果是
// DOM 里没有那个 id，行里控件的 .form 全是 null，点保存什么都不发生——
// 页面照常渲染，控制台一声不吭，只有真去点一次才会发现。
//
// 分类页就这么坏过一次：给表格加拖动排序时把它包进了 #reorder 表单，
// 从那一刻起每一行都存不下来了。正确的写法是把 <form> 放在表格外面，
// 行里的控件用 form="..." 指过去。
func TestNoFormInsideTableRow(t *testing.T) {
	names, err := fs.Glob(tmplFS, "templates/*.html")
	if err != nil || len(names) == 0 {
		t.Fatalf("读不到模板：%v", err)
	}
	// 只查**直接**挂在 <tr> 下的 <form>。放在 <td> 里是合法的，解析器
	// 照常收下——所以先把单元格的内容整个抠掉，剩下的才是行这一层。
	row := regexp.MustCompile(`(?s)<tr\b.*?</tr>`)
	cell := regexp.MustCompile(`(?s)<(td|th)\b.*?</(td|th)>`)
	for _, n := range names {
		body, err := fs.ReadFile(tmplFS, n)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range row.FindAllString(string(body), -1) {
			if bare := cell.ReplaceAllString(m, ""); strings.Contains(bare, "<form") {
				t.Errorf("%s 的 <tr> 里直接放了 <form>，解析时会被丢掉——"+
					"把它挪到 <table> 外面，行里用 form=\"id\" 指过去：\n%.160s", n, bare)
			}
		}
	}
}
