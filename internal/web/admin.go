package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cligc.com/internal/i18n"
	"cligc.com/internal/indexnow"
	"cligc.com/internal/store"
)

// redirectFlash 带一条提示信息跳转。
//
// flash 文本里常含空格和中文（错误信息直接透传给用户），必须经
// url.Values 编码——手工拼 "?flash="+msg 会产出带裸空格的 Location 头。
func redirectFlash(w http.ResponseWriter, r *http.Request, path, msg string) {
	if msg != "" {
		path += "?" + url.Values{"flash": {msg}}.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// postPath 返回某篇文章的后台编辑地址。
func postPath(id int64) string { return "/admin/edit/" + strconv.FormatInt(id, 10) }

// actorOf 构造一个 web 身份的 store.Actor。
func actorOf(r *http.Request) store.Actor {
	u := userFrom(r.Context())
	return store.Actor{UserID: u.ID, IsAdmin: u.IsAdmin(), Kind: "web"}
}

// handleAdminList 是后台首页，也是 AI 草稿的审核队列。
//
// 整个架构里 AI 只能写草稿，人在这里决定发不发——这道闸门是刻意的。
// 搜索引擎判定"规模化内容滥用"不看内容是不是 AI 写的，只看批量程度
// 和质量；把发布权留在人手里，是唯一可靠的约束。
func (s *Server) handleAdminList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	status := r.URL.Query().Get("status")
	lang := r.URL.Query().Get("lang")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	pg := pageParam(r)

	// 管理员看全站，作者只看自己的
	var scopeUser int64
	if !u.IsAdmin() {
		scopeUser = u.ID
	}

	data := map[string]any{"Status": status, "Query": q, "Page": pg, "Lang": lang}

	if q != "" {
		hits, total, err := s.db.Search(ctx, q, store.SearchFilter{
			UserID: scopeUser, Status: status, Lang: lang,
			Limit: s.cfg.PerPage, Offset: (pg - 1) * s.cfg.PerPage,
		})
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
			return
		}
		posts := make([]store.Post, 0, len(hits))
		for _, h := range hits {
			posts = append(posts, h.Post)
		}
		data["Posts"], data["Total"] = posts, total
	} else {
		posts, total, err := s.db.ListPosts(ctx, store.ListFilter{
			Status: status, UserID: scopeUser, Lang: lang,
			Limit: s.cfg.PerPage, Offset: (pg - 1) * s.cfg.PerPage,
		})
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
			return
		}
		data["Posts"], data["Total"] = posts, total
	}

	used, _ := s.db.PublishedToday(ctx, u.ID)
	featured, _ := s.db.CountFeatured(ctx)
	data["Featured"] = featured
	data["MaxFeatured"] = store.MaxFeatured
	data["PublishedToday"] = used
	data["DailyCap"] = s.cfg.DailyPublishCap
	data["CapLeft"] = s.cfg.DailyPublishCap - used

	// 语言筛选只在站上真有第二种语言的文章时出现。只写中文的站摆一个
	// 永远只有一个选项的下拉，是在为一个不存在的问题占地方。
	if langs, err := s.db.PostLangs(ctx, scopeUser); err == nil && len(langs) > 1 {
		data["Langs"] = langs
	}

	total, _ := data["Total"].(int)
	prev, next := pager("/admin", url.Values{"q": {q}, "status": {status}, "lang": {lang}},
		pg, s.cfg.PerPage, total)

	s.render(w, r, "admin_list.html", page{
		Title: s.tr(r, "admin.posts.title"), NoIndex: true, Wide: true, AdminTab: "posts",
		Flash: r.URL.Query().Get("flash"),
		Prev:  prev, Next: next, Data: data,
	})
}

func (s *Server) handleAdminNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "admin_edit.html", page{
		Title: s.tr(r, "admin.edit.newTitle"), NoIndex: true, Wide: true, AdminTab: "posts",
		Data: map[string]any{
			"Post": &store.Post{Indexable: true, Source: store.SourceHuman, Lang: i18n.Default().Code},
			"New":  true, "TransOf": "", "Cats": s.cats(r),
			"Media": s.coverChoices(r, nil), "CoverID": int64(0),
		},
	})
}

// coverChoices 是封面下拉里的候选图：最近上传的若干张。
//
// cur 是这篇文章当前的封面。它必须在列表里，哪怕已经翻出了"最近"的范围——
// 否则打开一篇旧文章，下拉里选不到它自己的封面，一保存封面就没了。
func (s *Server) coverChoices(r *http.Request, cur *int64) []store.Media {
	u := userFrom(r.Context())
	var scope int64
	if !u.IsAdmin() {
		scope = u.ID
	}
	items, err := s.db.ListMediaPage(r.Context(), scope, 60, 0)
	if err != nil {
		return nil
	}
	if cur == nil {
		return items
	}
	for _, m := range items {
		if m.ID == *cur {
			return items
		}
	}
	if m, err := s.db.MediaByID(r.Context(), *cur); err == nil {
		return append([]store.Media{*m}, items...)
	}
	return items
}

func (s *Server) handleAdminEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadOwned(w, r)
	if !ok {
		return
	}
	// 译文关联在界面上填的是"另一篇的 ID"，而库里存的是分组 key。
	// 这里反查同组里的任意一篇拿来回显——分组是对称的，显示哪一篇都对。
	transOf := ""
	if p.TransKey != "" {
		if others, err := s.db.Translations(r.Context(), p.TransKey, p.ID); err == nil && len(others) > 0 {
			transOf = strconv.FormatInt(others[0].ID, 10)
		}
	}
	s.render(w, r, "admin_edit.html", page{
		Title: s.tr(r, "admin.edit.editingFmt", p.Title), NoIndex: true, Wide: true, AdminTab: "posts", Flash: r.URL.Query().Get("flash"),
		Data: map[string]any{
			"Post": p, "TagsCSV": strings.Join(tagNamesOf(p), ", "), "TransOf": transOf,
			"TransCandidates": s.transCandidates(r, p),
			"Cats":            s.cats(r),
			"Media":           s.coverChoices(r, p.CoverMediaID),
			"CoverID":         deref(p.CoverMediaID),
		},
	})
}

// transCandidate 是"这篇是谁的译文"下拉里的一项。
type transCandidate struct {
	ID    int64
	Label string
}

// transCandidates 列出可以关联为同一内容的另一篇文章。
//
// 原先这里是让人手填文章 ID。ID 在界面上任何地方都不显示，等于要求作者
// 先去列表页数一遍或者翻 URL——多语言站上这是最常用的一个字段，不该
// 这么填。这里只列**别的语言**的文章：同语言的两篇不可能互为译文，
// 列出来只会让人选错。
func (s *Server) transCandidates(r *http.Request, p *store.Post) []transCandidate {
	u := userFrom(r.Context())
	if u == nil {
		return nil
	}
	posts, _, err := s.db.ListPosts(r.Context(), store.ListFilter{UserID: u.ID, Limit: 200})
	if err != nil {
		return nil
	}
	out := make([]transCandidate, 0, len(posts))
	for i := range posts {
		q := &posts[i]
		if q.ID == p.ID || q.Lang == p.Lang {
			continue
		}
		name := q.Lang
		if l, ok := i18n.ByCode(q.Lang); ok {
			name = l.Name
		}
		out = append(out, transCandidate{ID: q.ID, Label: name + " · " + q.Title})
	}
	return out
}

// cats 取板块列表给编辑器的下拉用。取不到就当没有板块——板块是可选的，
// 它挂了不该让整个编辑页打不开。
//
// 用后台那一份而不是按当前语言的：下拉里要列出**全部**板块，包括这个
// 语言下还一篇都没有的那些——不然给一篇英文文章根本选不到刚建的板块。
func (s *Server) cats(r *http.Request) []store.Category {
	c, err := s.db.ListCategoriesAdmin(r.Context(), i18n.Default().Code)
	if err != nil {
		return nil
	}
	return c
}

// loadOwned 取出文章并确认当前用户有权编辑。
func (s *Server) loadOwned(w http.ResponseWriter, r *http.Request) (*store.Post, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badID"))
		return nil, false
	}
	p, err := s.db.PostByID(r.Context(), id)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.postNotFound"))
		return nil, false
	}
	u := userFrom(r.Context())
	if !u.IsAdmin() && p.UserID != u.ID {
		s.renderError(w, r, http.StatusForbidden, s.tr(r, "err.forbidden"))
		return nil, false
	}
	return p, true
}

// formID 读一个表单里的数字 ID，读不出来就是 0。
func formID(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}

// deref 把可空的 ID 摊平成 0，给模板比较用——模板里没法解引用指针。
func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func tagNamesOf(p *store.Post) []string {
	out := make([]string, 0, len(p.Tags))
	for _, t := range p.Tags {
		out = append(out, t.Name)
	}
	return out
}

// splitTags 把逗号分隔的标签串切开，中英文逗号都认。
func splitTags(s string) []string {
	f := func(r rune) bool { return r == ',' || r == '，' || r == '、' }
	var out []string
	for _, p := range strings.FieldsFunc(s, f) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	idx := r.FormValue("indexable") != ""
	cover := formID(r, "cover_media_id")
	p, err := s.db.CreatePost(r.Context(), actorOf(r), store.CreatePostInput{
		Title: r.FormValue("title"), BodyMD: r.FormValue("body_md"),
		Slug: r.FormValue("slug"), Summary: r.FormValue("summary"),
		Tags: splitTags(r.FormValue("tags")), Source: r.FormValue("source"),
		CanonicalURL: r.FormValue("canonical_url"), Lang: r.FormValue("lang"), Indexable: &idx,
		CategorySlug: r.FormValue("category"),
		CoverMediaID: &cover, CoverAlt: r.FormValue("cover_alt"),
	})
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "flash.saveFailed", err.Error()))
		return
	}
	redirectFlash(w, r, postPath(p.ID), s.tr(r, "admin.edit.savedDraft"))
}

func (s *Server) handleAdminSave(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadOwned(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	get := func(k string) *string { v := r.FormValue(k); return &v }
	idx := r.FormValue("indexable") != ""
	tags := splitTags(r.FormValue("tags"))
	// 空串 = 取消封面。表单里的"无封面"那一项就是空串，和"没提交这个字段"
	// 在 HTML 表单里分不开——所以这里一律当成"用户表达了一个选择"。
	cover := formID(r, "cover_media_id")

	if _, err := s.db.UpdatePost(r.Context(), actorOf(r), p.ID, store.UpdatePostInput{
		Title: get("title"), BodyMD: get("body_md"), Slug: get("slug"),
		Summary: get("summary"), Tags: &tags, Source: get("source"),
		CanonicalURL: get("canonical_url"), Lang: get("lang"), Indexable: &idx,
		CategorySlug: get("category"),
		CoverMediaID: &cover, CoverAlt: get("cover_alt"),
	}); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "flash.saveFailed", err.Error()))
		return
	}
	// 译文关联：留空解除，填 ID 则并入那一组
	var ofID int64
	if v := strings.TrimSpace(r.FormValue("translation_of")); v != "" {
		ofID, _ = strconv.ParseInt(v, 10, 64)
	}
	if err := s.db.LinkTranslation(r.Context(), actorOf(r), p.ID, ofID); err != nil {
		redirectFlash(w, r, postPath(p.ID), s.tr(r, "flash.saveFailed", err.Error()))
		return
	}
	// 定时发布。datetime-local 给的是"本地墙上时间"，不带时区——按服务器
	// 本地时区解释。这是单人站的正确假设：排期的人和服务器通常是同一个
	// 时区，而多引入一个时区选择器只会让最常见的那种情况变复杂。
	msg := s.tr(r, "admin.edit.saved")
	var at *time.Time
	if v := strings.TrimSpace(r.FormValue("publish_at")); v != "" {
		if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
			at = &t
			msg = s.tr(r, "flash.scheduled", t.Format("2006-01-02 15:04"))
		}
	}
	if err := s.db.SetSchedule(r.Context(), actorOf(r), p.ID, at); err != nil {
		redirectFlash(w, r, postPath(p.ID), s.tr(r, "flash.saveFailed", err.Error()))
		return
	}
	redirectFlash(w, r, postPath(p.ID), msg)
}

func (s *Server) handleAdminPublish(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadOwned(w, r)
	if !ok {
		return
	}
	flash := s.tr(r, "flash.published")
	if _, err := s.db.PublishPost(r.Context(), actorOf(r), p.ID, s.cfg.DailyPublishCap); err != nil {
		flash = s.tr(r, "flash.publishFailed", err.Error())
	}
	redirectFlash(w, r, postPath(p.ID), flash)
}

func (s *Server) handleAdminUnpublish(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadOwned(w, r)
	if !ok {
		return
	}
	if _, err := s.db.UnpublishPost(r.Context(), actorOf(r), p.ID); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	redirectFlash(w, r, postPath(p.ID), s.tr(r, "flash.unpublished"))
}

// handleAdminFeature 切换首页置顶。
//
// 做成一个开关而不是 feature/unfeature 两条路由：置顶与否是一个二元状态，
// 让界面上只有一个按钮、由服务端读当前状态来决定方向，比让模板去分两种
// 情况渲染更不容易出错。
func (s *Server) handleAdminFeature(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadOwned(w, r)
	if !ok {
		return
	}
	on := !p.IsFeatured()
	if _, err := s.db.SetFeatured(r.Context(), actorOf(r), p.ID, on); err != nil {
		redirectFlash(w, r, backTo(r, "/admin"), s.tr(r, "flash.opFailed", err.Error()))
		return
	}
	msg := s.tr(r, "flash.unfeatured")
	if on {
		msg = s.tr(r, "flash.featured")
		if n, err := s.db.CountFeatured(r.Context()); err == nil && n > store.MaxFeatured {
			msg += " " + s.tr(r, "admin.posts.featuredOver",
				strconv.Itoa(n), strconv.Itoa(store.MaxFeatured))
		}
	}
	redirectFlash(w, r, backTo(r, "/admin"), msg)
}

// backTo 读取表单里的 return 字段决定跳回哪里，只接受站内相对路径。
// 没有它的话，从编辑页点置顶会被甩回列表页。
func backTo(r *http.Request, def string) string {
	v := r.FormValue("return")
	if v == "" {
		v = r.URL.Query().Get("return")
	}
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
		return v
	}
	return def
}

// handleAdminArchive 归档一篇文章：从列表、检索和 sitemap 里撤下，
// 但 URL 继续可访问。适合处理过时但被引用过的内容。
func (s *Server) handleAdminArchive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadOwned(w, r)
	if !ok {
		return
	}
	if _, err := s.db.SetStatus(r.Context(), actorOf(r), p.ID, store.StatusArchived); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	redirectFlash(w, r, postPath(p.ID), s.tr(r, "flash.archived"))
}

func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadOwned(w, r)
	if !ok {
		return
	}
	if err := s.db.DeletePost(r.Context(), actorOf(r), p.ID); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	redirectFlash(w, r, "/admin", s.tr(r, "flash.deleted"))
}

// --- API token 管理 ---

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	tokens, err := s.db.ListTokens(r.Context(), u.ID)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	s.render(w, r, "admin_tokens.html", page{
		Title: "API Token", NoIndex: true, Wide: true, AdminTab: "tokens", Flash: r.URL.Query().Get("flash"),
		Data: map[string]any{
			"Tokens":    tokens,
			"AllScopes": store.AllScopes, "ScopeGroups": store.ScopeGroups,
			// 新建成功后把明文带回来显示一次。放在查询串里是刻意的取舍：
			// 它只会出现在这一次跳转里，不落库、不进模板缓存；代价是可能
			// 进浏览器历史，所以页面上明确提示"只显示这一次"。
			"NewToken": r.URL.Query().Get("token"),
		},
	})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	u := userFrom(r.Context())
	var ttl time.Duration
	if d := r.FormValue("days"); d != "" && d != "0" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			ttl = time.Duration(n) * 24 * time.Hour
		}
	}
	plain, _, err := s.db.CreateToken(r.Context(), u.ID,
		r.FormValue("name"), r.Form["scopes"], ttl)
	if err != nil {
		redirectFlash(w, r, "/admin/tokens", s.tr(r, "admin.tokens.createFailed", err.Error()))
		return
	}
	http.Redirect(w, r, "/admin/tokens?"+url.Values{"token": {plain}}.Encode(), http.StatusSeeOther)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badID"))
		return
	}
	if err := s.db.RevokeToken(r.Context(), userFrom(r.Context()).ID, id); err != nil {
		redirectFlash(w, r, "/admin/tokens", s.tr(r, "flash.opFailed", err.Error()))
		return
	}
	redirectFlash(w, r, "/admin/tokens", s.tr(r, "admin.tokens.revokeDone"))
}

// handleSite 是站点接入设置页：搜索平台验证、IndexNow、GA4。
//
// 这几项刻意不做成命令行参数：它们是从各家网页控制台复制粘贴过来的凭据，
// 拿到就贴一次。要求站长开 shell 改启动参数再重启服务，这个流程本身就不对。
// 而 -addr / -db / -base-url 那些留在参数里——那是部署时定死的东西。
// 表单里按语言的站名/描述字段名，如 site_title:en。
const (
	fieldSiteTitle = "site_title:"
	fieldSiteDesc  = "site_desc:"
)

func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.db.Settings(ctx)
	def := i18n.Default().Code

	// 每个语言已发布多少篇。勾上一个语言等于告诉搜索引擎"这里有德语版"，
	// 所以得让人在勾之前看见那边到底有没有东西。
	counts, err := s.db.CountByLang(ctx)
	if err != nil {
		counts = map[string]int{}
	}

	// -lang-dir 词表自带 site.title 的语言，这两格填了也不生效。
	// 与其让人填完保存完再纳闷为什么页面没变，不如把话写在格子底下。
	titleFixed, descFixed := map[string]bool{}, map[string]bool{}
	for _, l := range i18n.Languages() {
		if _, ok := i18n.Own(l.Code, "site.title"); ok {
			titleFixed[l.Code] = true
		}
		if _, ok := i18n.Own(l.Code, "site.description"); ok {
			descFixed[l.Code] = true
		}
	}

	// 只开一种语言的站是常态。给它套上"每个语言一份"的那层外壳——
	// 语言小标题、左侧竖线、按语言分组的说明——是拿多语言站的代价
	// 去收多语言站的好处，而它一分好处也用不上。
	multi := 0
	for _, l := range i18n.ReadyLanguages() {
		if st.LangEnabled(l.Code, def) {
			multi++
		}
	}

	var onLangs, offLangs []i18n.Lang
	for _, l := range i18n.Languages() {
		if st.LangEnabled(l.Code, def) {
			onLangs = append(onLangs, l)
		} else {
			offLangs = append(offLangs, l)
		}
	}

	// 占位符显示"留空会变成什么"：其他语言回退到默认语言那份，
	// 默认语言自己回退到命令行的 -title / -desc。
	fbTitle, fbDesc := s.cfg.Title, s.cfg.Description
	if v := st.SiteTitles[def]; v != "" {
		fbTitle = v
	}
	if v := st.SiteDescs[def]; v != "" {
		fbDesc = v
	}

	s.render(w, r, "admin_site.html", page{
		Title: s.tr(r, "admin.site.title"), NoIndex: true, Wide: true, AdminTab: "site",
		Flash: r.URL.Query().Get("flash"),
		Data: map[string]any{
			"BaseURL":    s.cfg.BaseURL,
			"KeyFileURL": s.cfg.BaseURL + indexnow.KeyPath,
			// 命令行关死时把勾选框置灰：让人点一个点了不生效的开关，
			// 比不给这个开关更糟。
			"CommentsHardOff": !s.cfg.CommentsEnabled,
			// 开着的排前面、没开的在后面。一屏十一行勾选框里，"我开了哪几种"
			// 本来要一个一个找过去。
			"Langs":         i18n.Languages(),
			"LangsOn":       onLangs,
			"LangsOff":      offLangs,
			"DefaultLang":   def,
			"MultiLang":     multi > 1,
			"Counts":        counts,
			"TitleFixed":    titleFixed,
			"DescFixed":     descFixed,
			"FallbackTitle": fbTitle,
			"FallbackDesc":  fbDesc,
			"Me":            userFrom(ctx),
		},
	})
}

// handleSiteSave 保存站点设置的**某一块**。
//
// 这一页拆成了几张独立的表单（语言 / 站点信息 / 搜索平台 / 评论 /
// 统计），各自有自己的保存按钮。所以这里必须"改哪块动哪块"：从当前
// 设置出发，只覆盖这次提交的那一块。
//
// 不这么做的后果很具体：统计那张表单里没有 comments 字段，
// r.FormValue("comments") 得到空串，于是改一次 GA4 ID 就把评论关了。
// 和"单人站保存显示名会把简介抹掉"是同一类错，都不报错。
func (s *Server) handleSiteSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	in := s.db.Settings(r.Context())

	switch r.FormValue("section") {
	case "langs":
		// 表单只能表达"翻译达标的语言"——没达标的根本没渲染成勾选框。
		// 直接按表单覆盖的话，一个因为词表改动暂时掉到门槛以下的语言，
		// 会在下一次随便保存点别的时被无声关掉。
		enabled := r.Form["langs"]
		inForm := map[string]bool{}
		for _, l := range i18n.ReadyLanguages() {
			inForm[l.Code] = true
		}
		for _, code := range in.EnabledLangs {
			if !inForm[code] {
				enabled = append(enabled, code)
			}
		}
		in.EnabledLangs = enabled

	case "copy":
		titles, descs := map[string]string{}, map[string]string{}
		for _, l := range i18n.Languages() {
			titles[l.Code] = r.FormValue(fieldSiteTitle + l.Code)
			descs[l.Code] = r.FormValue(fieldSiteDesc + l.Code)
		}
		in.SiteTitles, in.SiteDescs = titles, descs

	case "seo":
		in.GoogleVerify = r.FormValue("google_verify")
		in.BingVerify = r.FormValue("bing_verify")
		in.IndexNowKey = r.FormValue("indexnow_key")

	case "comments":
		in.CommentsEnabled = r.FormValue("comments") != ""

	case "analytics":
		in.GA4ID = r.FormValue("ga4_id")

	default:
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}

	if err := s.db.SaveSettings(r.Context(), in); err != nil {
		redirectFlash(w, r, "/admin/site", s.tr(r, "flash.saveFailed", err.Error()))
		return
	}
	// 提交器拿的是内存里的 key，存完要同步过去，否则下一次发布还在用旧的。
	s.cfg.IndexNow.SetKey(in.IndexNowKey)
	redirectFlash(w, r, "/admin/site", s.tr(r, "admin.site.saved"))
}
