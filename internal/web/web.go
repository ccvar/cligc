// Package web 提供服务端渲染的页面和一个极简后台。
//
// 全站没有前端框架，也没有客户端路由：每个 URL 都由服务器吐出完整 HTML。
// 这不只是"轻量"的偏好——对一个指望被搜索引擎收录的内容站，它是决定性的。
// 纯客户端渲染的页面要走 Googlebot 的二次渲染队列，页面一多就基本抓不动；
// 服务端渲染没有这个问题。
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cligc.com/internal/i18n"
	"cligc.com/internal/indexnow"
	"cligc.com/internal/store"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

// Config 是站点级设置。
type Config struct {
	BaseURL     string // 绝对地址，如 https://cligc.com（无尾斜杠）
	Title       string
	Description string
	MediaRoot   string
	// ImageMaxDim 是上传图片的长边上限，超过就等比缩小。<=0 表示不缩。
	ImageMaxDim     int
	DailyPublishCap int
	PerPage         int

	// TrustProxy 决定是否相信 X-Forwarded-For。只有确实部署在自己的
	// 反向代理后面时才打开——否则任何人都能伪造来源 IP 绕过登录限速。
	TrustProxy bool

	// CommentsEnabled 控制是否开放评论。关掉之后表单和提交路由都不存在，
	// 而不只是把表单藏起来——藏起来的表单仍然可以被直接 POST。
	CommentsEnabled bool

	// CommentQueueCap 是待审队列的容量上限，<=0 表示不限。
	//
	// 限速只管住"单个来源发得多快"，管不住"一万个来源各发三条"。队列被
	// 灌满之后真正的损失不是磁盘，是一万条垃圾里混着的三条真人留言再也
	// 找不出来——评论功能等于废了。满了就先拒收新的匿名评论，已经在队列
	// 里的不受影响，站长清完就自动恢复。
	CommentQueueCap int

	// IndexNow 是发布后主动通知搜索引擎的提交器。key 存在库里、后台可改，
	// 这里只需要拿到它好在设置页显示状态并同步新 key。
	IndexNow *indexnow.Pinger
}

// Server 持有页面层的依赖。
type Server struct {
	db  *store.DB
	cfg Config
	// 每个语言一套模板，启动时各构建一次。
	//
	// 这样 {{t "key"}} 在任何地方（包括 partial）都能直接用，不需要把页面
	// 对象一路传下去写成 {{$.T "key"}}。代价是几百 KB 内存，换来的是模板里
	// 完全看不到 i18n 的管道。
	tmpl  map[string]map[string]*template.Template
	media http.Handler
	login *attemptLimiter

	// assets 是每个内嵌静态文件的内容指纹，用来给 URL 加版本号。
	//
	// 需要它是因为 embed.FS 里的文件 modtime 是零值，http.FileServer
	// 因此既发不出 Last-Modified 也发不出 ETag——浏览器既无法条件请求、
	// 也无法安全缓存，结果是每次访问都把整个 CSS 和 JS 重下一遍。
	assets map[string]string
}

// New 构造页面服务并预编译模板。模板在启动时一次性解析，
// 解析失败直接 panic —— 模板语法错误应当在启动时暴露，而不是在某个
// 冷门页面第一次被访问时才 500。
func New(db *store.DB, cfg Config) (*Server, error) {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.PerPage <= 0 {
		cfg.PerPage = 20
	}
	if cfg.CommentQueueCap == 0 {
		cfg.CommentQueueCap = 500
	}
	s := &Server{db: db, cfg: cfg,
		tmpl: map[string]map[string]*template.Template{}, login: newAttemptLimiter()}

	pages, err := fs.Glob(tmplFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, lang := range i18n.Languages() {
		set := map[string]*template.Template{}
		// 外壳和片段库不是页面，跳过。片段分两个文件：partials.html 是两边
		// 共用的（翻页、空状态、图标、字标、后台标签页），partials_site.html
		// 只给公开页用（文章卡片、精选卡、侧栏）——和 site.css / site.js 对齐。
		const layout = "templates/layout.html"
		shared := []string{layout, "templates/partials.html", "templates/partials_site.html"}
		for _, p := range pages {
			name := strings.TrimPrefix(p, "templates/")
			if name == "layout.html" || strings.HasPrefix(name, "partials") {
				continue
			}
			t, err := template.New("layout.html").Funcs(s.funcs(lang)).
				ParseFS(tmplFS, append(append([]string{}, shared...), p)...)
			if err != nil {
				return nil, fmt.Errorf("parse %s (%s): %w", name, lang.Code, err)
			}
			set[name] = t
		}
		s.tmpl[lang.Code] = set
	}

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s.media = http.StripPrefix("/static/", immutable(http.FileServer(http.FS(sub))))

	if s.assets, err = fingerprint(sub); err != nil {
		return nil, err
	}
	return s, nil
}

// fingerprint 给每个静态文件算一段内容哈希。
//
// 指纹进 URL 之后，内容一变 URL 就变，于是可以放心给静态资源打
// immutable 长缓存：老 URL 不会再被请求，新 URL 必然是新内容。
// 这比"短缓存 + 每次条件请求"省一个往返，也不会有部署后拿到旧 CSS 的问题。
func fingerprint(root fs.FS) (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[p] = hex.EncodeToString(sum[:])[:10]
		return nil
	})
	return out, err
}

// immutable 给静态资源打长缓存。只有在 URL 带内容指纹时这样做才安全，
// 见 fingerprint。
func immutable(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}

// Routes 注册所有页面路由。
func (s *Server) Routes(mux *http.ServeMux) {
	// 兜底：未匹配的路径走样式化的 404 页，而不是 net/http 的裸文本。
	// Go 1.22 起 ServeMux 按"最具体的模式优先"匹配，"/" 是最不具体的，
	// 所以它不会抢走下面任何一条路由。
	mux.HandleFunc("/", s.handleNotFound)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// 浏览器在每次首访时都会自动请求 /favicon.ico。没有这条路由的话，
	// 它会落到兜底的 404，返回整整一页 HTML——每个访客都白下载一次。
	// 真正的图标由 <link rel="icon"> 指向 /static/favicon.svg。
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800")
		w.WriteHeader(http.StatusNoContent)
	})

	// 公开页面
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /p/{slug}", s.handlePost)
	mux.HandleFunc("GET /c/{slug}", s.handleCategory)
	mux.HandleFunc("GET /t/{tag}", s.handleTag)
	mux.HandleFunc("GET /u/{slug}", s.handleAuthor)
	mux.HandleFunc("GET /search", s.handleSearch)

	// SEO
	mux.HandleFunc("GET /sitemap.xml", s.handleSitemap)
	mux.HandleFunc("GET /sitemaps/{n}", s.handleSitemapChunk)
	mux.HandleFunc("GET /robots.txt", s.handleRobots)
	mux.HandleFunc("GET /feed.xml", s.handleFeed)

	// 静态与上传
	mux.Handle("GET /static/", s.media)
	mux.Handle("GET /media/", http.StripPrefix("/media/",
		cacheForever(http.FileServer(http.Dir(s.cfg.MediaRoot)))))

	// 登录
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// 后台（全部需要登录）
	a := s.requireLogin
	mux.Handle("GET /admin", a(s.handleAdminList))
	mux.Handle("GET /admin/new", a(s.handleAdminNew))
	mux.Handle("GET /admin/edit/{id}", a(s.handleAdminEdit))
	mux.Handle("POST /admin/posts", a(s.handleAdminCreate))
	mux.Handle("POST /admin/posts/{id}", a(s.handleAdminSave))
	mux.Handle("POST /admin/posts/{id}/publish", a(s.handleAdminPublish))
	mux.Handle("POST /admin/posts/{id}/unpublish", a(s.handleAdminUnpublish))
	mux.Handle("POST /admin/posts/{id}/archive", a(s.handleAdminArchive))
	mux.Handle("POST /admin/posts/{id}/feature", a(s.handleAdminFeature))
	mux.Handle("POST /admin/posts/{id}/delete", a(s.handleAdminDelete))
	mux.Handle("GET /admin/tokens", a(s.handleTokens))
	mux.Handle("POST /admin/tokens", a(s.handleCreateToken))
	mux.Handle("POST /admin/tokens/{id}/revoke", a(s.handleRevokeToken))

	mux.Handle("GET /admin/media", a(s.handleMediaList))
	mux.Handle("POST /admin/media", a(s.handleMediaUpload))
	mux.Handle("POST /admin/media/{id}/delete", a(s.handleMediaDelete))
	// 编辑器里的选图弹窗和拖拽/粘贴上传。走 JSON 而不是表单跳转：
	// 这两个动作要的是"刚存下来的图在哪"，好当场插进正文。
	mux.Handle("GET /admin/media.json", a(s.handleMediaPick))
	mux.Handle("POST /admin/media.json", a(s.handleMediaPickUpload))

	mux.Handle("GET /admin/categories", a(s.handleCategories))
	mux.Handle("POST /admin/categories", a(s.handleCategorySave))
	mux.Handle("POST /admin/categories/{id}/delete", a(s.handleCategoryDelete))
	mux.Handle("POST /admin/categories/reorder", a(s.handleCategoryReorder))
	mux.Handle("GET /admin/site", a(s.handleSite))
	mux.Handle("GET /admin/skill-pack", a(s.handleSkillPack))
	mux.Handle("POST /admin/site", a(s.handleSiteSave))
	mux.Handle("GET /admin/profile", a(s.handleProfile))
	mux.Handle("POST /admin/profile", a(s.handleProfileSave))
	mux.Handle("POST /admin/profile/password", a(s.handleChangePassword))

	// 评论开关现在在后台设置页，随时可改，所以路由必须常驻，由 handler
	// 在每次请求时查一遍设置。
	//
	// 原先是"关掉就不注册路由"。那样更干净，但代价是改一次开关要重启服务。
	// 换成常驻之后，关掉的效果不变——handler 第一件事就是查开关然后 404，
	// 而不是"表单藏起来但接口还在"。有测试守着这一条。
	mux.HandleFunc("POST /p/{slug}/comments", s.handleCommentSubmit)
	mux.Handle("GET /admin/comments", a(s.handleCommentQueue))
	mux.Handle("POST /admin/comments/{id}/{action}", a(s.handleCommentModerate))
}

// commentsOn 报告当前是否开放评论。
//
// 命令行的 -comments=false 是一道硬开关：设了它，后台里怎么勾都打不开。
// 这样容器化部署可以把"这个站不要评论"焊死，而不是寄希望于没人去点。
func (s *Server) commentsOn(r *http.Request) bool {
	return s.cfg.CommentsEnabled && s.db.Settings(r.Context()).CommentsEnabled
}

// page 是每个页面都要填的公共部分。
//
// NoIndex 和 Canonical 是这个结构体存在的主要理由：一个 UGC 站最常见的
// 索引问题不是"页面没被抓到"，而是"太多低价值页面被抓到"，把整站的
// 质量评分拖下去。哪些页面不该进索引，必须在每个 handler 里显式表态。
type page struct {
	Site      Config
	Wide      bool // 后台页面用更宽的容器
	Reading   bool // 文章页：容器加宽以容纳右侧大纲，但正文栏宽不变
	Title     string
	Desc      string
	Canonical string
	NoIndex   bool
	IsArticle bool // 决定 og:type，文章页与聚合页的社交卡片语义不同

	// Lang 由 render 按请求填充，用于 <html lang> 和 hreflang。
	Lang i18n.Lang
	// SiteTitle / SiteDesc 是按语言解析后的站名与描述，模板一律用它们，
	// 不要直接读 .Site.Title——那是未经语言覆盖的配置原值。
	SiteTitle string
	SiteDesc  string
	// BrandMark 为真时用 SVG 字标渲染 cligc，BrandText 是站名的其余部分。
	// 站名不以 cligc 开头时 BrandMark 为假、BrandText 是整个站名。
	BrandMark bool
	BrandText string

	// Settings 是后台设置页里那几项（验证码、GA4、IndexNow）。
	// 每次渲染都要读——它们决定 <head> 里出不出那几个 meta。
	Settings store.SiteSettings

	// Categories 是公开页的板块导航。为空时那条导航整个不渲染。
	Categories []store.Category

	// CommentsOn 决定后台标签栏里出不出"评论"那一项。
	CommentsOn bool

	// MultiAuthor 为真时，署名才链到作者页。
	//
	// 单人站上 /u/{slug} 列的和首页是同一批文章——对读者是一条通向
	// 同样内容的多余链接，对搜索引擎是重复内容。所以单人站不链它，
	// 但路由留着：已经存在的外链和书签不该因此断掉。
	MultiAuthor bool

	// IsAdmin 决定加载 admin.css/js 还是 site.css/js。
	//
	// 由路径推导而不是让每个 handler 自己设：靠人记得设标志的方案，
	// 迟早会有一个新页面忘了设，而表现是"这一页样式全丢了"。
	IsAdmin bool
	// Image 是分享到社交平台时那张大图的绝对地址，空则不发 og:image。
	// ImageAlt 跟着它走：发了图不给 alt，读屏软件念出来的是一串文件名。
	Image    string
	ImageAlt string

	// Alternates 是同一内容的其它语言版本。
	//
	// 三条规则错一条整套失效：必须**互相**引用（每个版本列出所有版本，
	// 包括它自己）、必须有 x-default、且每个版本的 canonical 必须指向
	// 它自己——把译文的 canonical 指向原文等于告诉 Google 别收录译文。
	Alternates []Alternate

	// 后台导航状态。AdminTab 非空时 render 会顺带查一次待审评论数，
	// 让"有东西等着你处理"这件事在每个后台页面都看得见——审核队列
	// 最大的失败模式不是难用，是没人想起来去看。
	AdminTab        string
	PendingComments int
	Prev            string
	Next            string
	User            *store.User
	Flash           string
	Data            any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	lang := LangFrom(r.Context())
	t, ok := s.tmpl[lang.Code][name]
	if !ok {
		http.Error(w, "template not found: "+name, http.StatusInternalServerError)
		return
	}
	p.Lang = lang
	p.Site = s.cfg
	if p.User == nil {
		p.User = userFrom(r.Context())
	}
	p.Settings = s.db.Settings(r.Context())

	// 站名和描述的优先级：词表 > 后台设置 > 命令行。
	//
	//   命令行  部署时定的兜底值
	//   后台    站长随时改的，按语言各存一份，多数站只用到这一层
	//   词表    把文案跟着翻译一起进版本库时用，放自己的 -lang-dir 目录
	//
	// 内置词表**不带** site.title / site.description。带了的话，站长在
	// 后台里输入的名字会被内置文案无声盖掉——那是在替别人的站做主。
	//
	// 词表这一层用 Own 而不是 T：T 会回退到默认语言，于是 -lang-dir 里
	// 只给中文写了一条 site.title，英文页就会拿中文那条去盖掉站长在后台
	// 填的英文站名。词表只应该管它自己声明了的那个语言。
	def := i18n.Default().Code
	siteTitle := s.cfg.Title
	if v := p.Settings.SiteTitleFor(lang.Code, def); v != "" {
		siteTitle = v
	}
	if v, ok := i18n.Own(lang.Code, "site.title"); ok {
		siteTitle = v
	}

	p.SiteDesc = s.cfg.Description
	if v := p.Settings.SiteDescFor(lang.Code, def); v != "" {
		p.SiteDesc = v
	}
	if v, ok := i18n.Own(lang.Code, "site.description"); ok {
		p.SiteDesc = v
	}

	p.SiteTitle = siteTitle
	p.BrandMark, p.BrandText = splitBrand(siteTitle)
	p.CommentsOn = s.commentsOn(r)
	if n, err := s.db.CountUsers(r.Context()); err == nil {
		p.MultiAuthor = n > 1
	}
	// 语言前缀已经被 WithLang 剥掉，这里看到的是规范化后的路径。
	p.IsAdmin = isAdminPath(r.URL.Path)
	if !p.IsAdmin {
		p.Categories, _ = s.db.ListCategories(r.Context())
	}

	// 站名就是光秃秃一个 cligc 时，按界面语言补上本地文字的副名。
	// 条件卡得很死，是为了不替别人做主：
	//   -title "cligc"        → 补（汉字圈出「轻格 / 輕格 / 軽格 / 경격」）
	//   -title "cligc 我的站"  → 不补，运营方自己起的副名原样保留
	//   -title "我的博客"      → 连字标都不出，i18n 完全不插手
	if p.BrandMark && p.BrandText == "" {
		// 用 Own 而不是 T：这里不能回退到默认语言，否则德语页面会挂上中文副名。
		if v, ok := i18n.Own(lang.Code, "brand.companion"); ok {
			p.BrandText = v
			// 连 siteTitle 一起改：下面的 <title>、og:site_name、RSS 标题
			// 都从它取值，只改 p.SiteTitle 会让标签页标题少掉副名。
			siteTitle += " " + v
			p.SiteTitle = siteTitle
		}
	}

	// 没有单独描述的页面退回站点描述。这是 meta description 的通行默认，
	// 也让"按语言覆盖"对首页同样生效。
	if p.Desc == "" {
		p.Desc = p.SiteDesc
	}
	if p.Title == "" {
		p.Title = siteTitle
	} else {
		p.Title = p.Title + " · " + siteTitle
	}
	if p.Canonical == "" {
		p.Canonical = s.cfg.BaseURL + r.URL.Path
	}
	if p.AdminTab != "" && s.commentsOn(r) {
		p.PendingComments, _ = s.db.CountComments(r.Context(), store.CommentPending)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, p); err != nil {
		// 响应头已经发出去了，这里只能记录，不能再改状态码
		fmt.Printf("template %s: %v\n", name, err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.WriteHeader(code)
	s.render(w, r, "error.html", page{
		Title:   fmt.Sprintf("%d", code),
		NoIndex: true,
		Data:    map[string]any{"Code": code, "Message": msg},
	})
}

// funcs 是模板里可用的辅助函数。每个语言绑一份，所以 t/tn/lurl 不需要
// 在调用点再传语言。
func (s *Server) funcs(lang i18n.Lang) template.FuncMap {
	return template.FuncMap{
		// t 取一条文案；tn 带数量（处理复数）；tf 按位置替换 {0} {1}…
		"t":  func(key string) string { return i18n.T(lang.Code, key) },
		"tn": func(key string, n int) string { return i18n.N(lang.Code, key, n) },
		"tf": func(key string, args ...string) string { return i18n.F(lang.Code, key, args...) },
		// lurl 给站内路径加当前语言的前缀。模板里凡是写死的站内链接都要经过它，
		// 否则在 /en/ 下点一下就掉回中文站了。
		"lurl": func(path string) string { return langPath(lang, path) },
		"lang": func() i18n.Lang { return lang },
		// langs 只给**达标**的语言：用户选了一种语言结果看到一半中文，
		// 比根本没有那个选项更糟——他会以为站点坏了。
		// 切换器只列站点实际提供的语言。ReadyLanguages 是"翻译够完整"，
		// 和"这个站对外提供它"是两回事——前者是词表的属性，后者是站长的决定。
		"langs": func() []i18n.Lang { return s.enabledLangs(context.Background()) },

		"itoa":     strconv.Itoa,
		"itoa64":   func(n int64) string { return strconv.FormatInt(n, 10) },
		"xdefault": xDefaultOf,
		// altHref 算"切到另一种语言时该去哪"。文章页有对应译文就跳译文，
		// 否则跳那个语言的首页——"这个标签在英文站的对应页"通常并不存在，
		// 硬跳过去只会得到一个空列表，不如老老实实回首页。
		"altHref": func(p page, code string) string {
			for _, a := range p.Alternates {
				if a.Code == code {
					return a.URL
				}
			}
			if l, ok := i18n.ByCode(code); ok {
				return langPath(l, "/")
			}
			return "/"
		},
		"safeHTML": func(v string) template.HTML {
			// 只用于两处可信来源：goldmark 渲染的正文（原始 HTML 已转义），
			// 以及 render.Highlight 产出的检索摘要（自行转义后才插入 <mark>）。
			return template.HTML(v)
		},
		"date":    func(t time.Time) string { return t.Format("2006-01-02") },
		"rfc3339": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"datep": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Format("2006-01-02")
		},
		"urlq": url.QueryEscape,
		"urlp": url.PathEscape,
		"join": strings.Join,
		"tagNames": func(tags []store.Tag) []string {
			out := make([]string, 0, len(tags))
			for _, t := range tags {
				out = append(out, t.Name)
			}
			return out
		},
		"add": func(a, b int) int { return a + b },

		// readtime 按约 400 字/分钟估算。给读者一个"要花多久"的预期，
		// 比单纯的字数更有用。
		//
		// 中文按字、英文按词，两者的 400/分钟恰好都是合理量级，所以这里
		// 不按语言分算——真要精确到分钟级，估算本身的误差比语言差异更大。
		"readtime": func(words int) string {
			m := (words + 399) / 400
			if m < 1 {
				m = 1
			}
			return i18n.N(lang.Code, "common.readtime", m)
		},

		// asset 把静态资源名转成带内容指纹的 URL，如
		// asset "app.css" -> /static/app.css?v=1f3c9ab204
		"asset": func(name string) string {
			if v, ok := s.assets[name]; ok {
				return "/static/" + name + "?v=" + v
			}
			return "/static/" + name
		},

		// filesize 把字节数格式化成人能读的大小。
		"filesize": func(n int64) string {
			switch {
			case n >= 1<<20:
				return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
			case n >= 1<<10:
				return strconv.FormatInt(n/(1<<10), 10) + " KB"
			default:
				return strconv.FormatInt(n, 10) + " B"
			}
		},

		// seq 生成 0..n-1，用于渲染配额小格。
		// 模板里画离散格子而不是用百分比宽度的进度条，是因为 CSP 禁内联样式，
		// 没法在模板里写 style="width:60%"。
		"seq": func(n int) []int {
			if n < 0 {
				n = 0
			}
			if n > 50 { // 配额设得离谱时别画出几百个格子
				n = 50
			}
			out := make([]int, n)
			for i := range out {
				out[i] = i
			}
			return out
		},
		// dict 让 partial 能接收具名参数，而不是被迫共用顶层 . 的结构。
		// Go 的模板没有内置这个，但没有它，每个 partial 都得配一个
		// 专用的 Go 结构体，得不偿失。
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict 需要偶数个参数")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict 的第 %d 个键不是字符串", i)
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		// 这三个把库里的枚举值翻成给人看的标签。必须走词表：
		// 它们和模板里的 {{t "source.human"}} 渲染在同一行徽章上，
		// 写死中文会让非中文界面出现中一句外一句。
		"statusLabel": func(s string) string {
			switch s {
			case store.StatusPublished:
				return i18n.T(lang.Code, "status.published")
			case store.StatusArchived:
				return i18n.T(lang.Code, "status.archived")
			default:
				return i18n.T(lang.Code, "status.draft")
			}
		},
		"commentStatus": func(s string) string {
			switch s {
			case store.CommentApproved:
				return i18n.T(lang.Code, "admin.comments.statusApproved")
			case store.CommentSpam:
				return i18n.T(lang.Code, "admin.comments.statusSpam")
			default:
				return i18n.T(lang.Code, "admin.comments.statusPending")
			}
		},
		// scopeDesc 把 token 权限码翻成人话。"posts:read" 对写代码的人是
		// 自明的，对在后台勾选框的人不是——他要判断的是"给了这个，AI 能干
		// 什么"，而不是记住一套命名约定。
		"scopeDesc": func(sc string) string {
			switch sc {
			case "posts:read":
				return i18n.T(lang.Code, "scope.read")
			case "posts:write":
				return i18n.T(lang.Code, "scope.write")
			case "posts:publish":
				return i18n.T(lang.Code, "scope.publish")
			case "media:write":
				return i18n.T(lang.Code, "scope.media")
			case "media:delete":
				return i18n.T(lang.Code, "scope.mediaDelete")
			case "comments:moderate":
				return i18n.T(lang.Code, "scope.comments")
			case "tokens:manage":
				return i18n.T(lang.Code, "scope.tokens")
			case "site:admin":
				return i18n.T(lang.Code, "scope.site")
			}
			return ""
		},
		"sourceLabel": func(s string) string {
			switch s {
			case store.SourceAIAssisted:
				return i18n.T(lang.Code, "source.aiAssist")
			case store.SourceAIGenerated:
				return i18n.T(lang.Code, "source.aiGen")
			default:
				return i18n.T(lang.Code, "source.human")
			}
		},
	}
}

// cacheForever 给内容寻址的上传文件加长缓存。文件名是内容哈希，
// 内容变了文件名就变了，所以可以放心设一年。
//
// 同时打上 nosniff：上传目录里全是用户内容，绝不能让浏览器根据
// 字节内容自行推断出 text/html 之类的类型并执行它。
func cacheForever(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

// pageParam 返回 1 起算的页码。
func pageParam(r *http.Request) int {
	n := 1
	if v := r.URL.Query().Get("page"); v != "" {
		fmt.Sscanf(v, "%d", &n)
	}
	if n < 1 {
		n = 1
	}
	if n > 500 { // 深翻页对人和爬虫都没意义，直接封顶
		n = 500
	}
	return n
}

// pager 计算上一页/下一页链接，没有则返回空串。
//
// keep 是需要在翻页时保留的其它查询参数（搜索词、状态筛选……），
// 由 url.Values 统一编码。早先的版本手工拼 "?q="+词，用户一旦搜了
// 含 & 或空格的词，翻页链接就坏了——这类 bug 不报错，只是默默失效。
func pager(path string, keep url.Values, page, perPage, total int) (prev, next string) {
	build := func(n int) string {
		v := url.Values{}
		for k, vals := range keep {
			for _, s := range vals {
				if s != "" {
					v.Add(k, s)
				}
			}
		}
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		if len(v) == 0 {
			return path
		}
		return path + "?" + v.Encode()
	}
	if page > 1 {
		prev = build(page - 1)
	}
	if page*perPage < total {
		next = build(page + 1)
	}
	return
}

// Alternate 是同一内容的一个语言版本，用于输出 hreflang。
type Alternate struct {
	Code string // hreflang 值
	URL  string // 绝对地址
	Name string // 语言切换器上显示的名字
	Self bool   // 是否就是当前这一版
}

// alternatesFor 为一篇文章算出全套 hreflang 条目。
//
// 包含自身：hreflang 必须是互相引用的完整集合，漏掉自引用会让 Google
// 整体忽略这组标注——而且不报错，只是安静地当它不存在。
func (s *Server) alternatesFor(p *store.Post, others []store.Post) []Alternate {
	if len(others) == 0 {
		return nil
	}
	mk := func(q *store.Post, self bool) Alternate {
		l, _ := i18n.ByCode(q.Lang)
		return Alternate{
			Code: q.Lang,
			URL:  s.cfg.BaseURL + langPath(l, "/p/"+q.Slug),
			Name: l.Name,
			Self: self,
		}
	}
	out := []Alternate{mk(p, true)}
	for i := range others {
		out = append(out, mk(&others[i], false))
	}
	return out
}

// homeAlternates 给首页算各语言版本，用来发 hreflang。
//
// 文章页的对应关系靠译文分组一篇篇连起来，首页没有那种东西——但它的
// 对应关系是定义出来的：站点开了哪些语言，那几个首页就互为版本。
//
// 这件事直到语言由站长显式声明才成立。在那之前"站上有哪些语言"是词表
// 的属性，照着发 hreflang 等于替十种语言的空列表页做担保。
func (s *Server) homeAlternates(ctx context.Context, cur i18n.Lang) []Alternate {
	langs := s.enabledLangs(ctx)
	if len(langs) < 2 {
		return nil // 只有一种语言，没有"其它版本"这回事
	}
	out := make([]Alternate, 0, len(langs))
	for _, l := range langs {
		out = append(out, Alternate{
			Code: l.Code,
			URL:  s.cfg.BaseURL + langPath(l, "/"),
			Name: l.Name,
			Self: l.Code == cur.Code,
		})
	}
	return out
}

// xDefaultOf 返回 hreflang="x-default" 该指向哪一版：默认语言的那一版，
// 没有就退回第一个。
func xDefaultOf(alts []Alternate) string {
	for _, a := range alts {
		if a.Code == i18n.Default().Code {
			return a.URL
		}
	}
	if len(alts) > 0 {
		return alts[0].URL
	}
	return ""
}

// splitBrand 把站名拆成"要不要用字标"和"字标之后的文字"。
//
// 站名以 cligc 开头就用 SVG 字标画那几个字母，剩下的（如「轻格」）走文字；
// 否则整串都走文字。这样 fork 这个项目的人改个名字就够了，不用动模板，
// 也不会出现一个画着 cligc 却叫别的名字的站。
func splitBrand(title string) (bool, string) {
	const brand = "cligc"
	t := strings.TrimSpace(title)
	if len(t) < len(brand) || !strings.EqualFold(t[:len(brand)], brand) {
		return false, t
	}
	return true, strings.TrimSpace(t[len(brand):])
}

// enabledLangs 返回站点实际对外提供的语言，按词表顺序。
//
// 和 i18n.ReadyLanguages() 的区别：那个回答"翻译够不够完整"，是词表的属性；
// 这个回答"站长愿不愿意对外提供"，是站点的决定。只写中文的站不该给 11 种
// 语言都发 hreflang——那些页面是空的。
func (s *Server) enabledLangs(ctx context.Context) []i18n.Lang {
	st := s.db.Settings(ctx)
	def := i18n.Default().Code
	var out []i18n.Lang
	for _, l := range i18n.ReadyLanguages() {
		if st.LangEnabled(l.Code, def) {
			out = append(out, l)
		}
	}
	return out
}
