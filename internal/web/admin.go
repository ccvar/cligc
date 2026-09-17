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
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	pg := pageParam(r)

	// 管理员看全站，作者只看自己的
	var scopeUser int64
	if !u.IsAdmin() {
		scopeUser = u.ID
	}

	data := map[string]any{"Status": status, "Query": q, "Page": pg}

	if q != "" {
		hits, total, err := s.db.Search(ctx, q, store.SearchFilter{
			UserID: scopeUser, Status: status,
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
			Status: status, UserID: scopeUser,
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

	total, _ := data["Total"].(int)
	prev, next := pager("/admin", url.Values{"q": {q}, "status": {status}},
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
		},
	})
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

// cats 取分类列表给模板用。取不到就当没有分类——分类是可选的，
// 它挂了不该让整个编辑页打不开。
func (s *Server) cats(r *http.Request) []store.Category {
	c, err := s.db.ListCategories(r.Context())
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
	p, err := s.db.CreatePost(r.Context(), actorOf(r), store.CreatePostInput{
		Title: r.FormValue("title"), BodyMD: r.FormValue("body_md"),
		Slug: r.FormValue("slug"), Summary: r.FormValue("summary"),
		Tags: splitTags(r.FormValue("tags")), Source: r.FormValue("source"),
		CanonicalURL: r.FormValue("canonical_url"), Lang: r.FormValue("lang"), Indexable: &idx,
		CategorySlug: r.FormValue("category"),
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

	if _, err := s.db.UpdatePost(r.Context(), actorOf(r), p.ID, store.UpdatePostInput{
		Title: get("title"), BodyMD: get("body_md"), Slug: get("slug"),
		Summary: get("summary"), Tags: &tags, Source: get("source"),
		CanonicalURL: get("canonical_url"), Lang: get("lang"), Indexable: &idx,
		CategorySlug: get("category"),
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
func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "admin_site.html", page{
		Title: s.tr(r, "admin.site.title"), NoIndex: true, Wide: true, AdminTab: "site",
		Flash: r.URL.Query().Get("flash"),
		Data: map[string]any{
			"BaseURL":    s.cfg.BaseURL,
			"KeyFileURL": s.cfg.BaseURL + indexnow.KeyPath,
			// 命令行关死时把勾选框置灰：让人点一个点了不生效的开关，
			// 比不给这个开关更糟。
			"CommentsHardOff": !s.cfg.CommentsEnabled,
		},
	})
}

func (s *Server) handleSiteSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	in := store.SiteSettings{
		CommentsEnabled: r.FormValue("comments") != "",
		GoogleVerify:    r.FormValue("google_verify"),
		BingVerify:      r.FormValue("bing_verify"),
		GA4ID:           r.FormValue("ga4_id"),
		IndexNowKey:     r.FormValue("indexnow_key"),
	}
	if err := s.db.SaveSettings(r.Context(), in); err != nil {
		redirectFlash(w, r, "/admin/site", s.tr(r, "flash.saveFailed", err.Error()))
		return
	}
	// 提交器拿的是内存里的 key，存完要同步过去，否则下一次发布还在用旧的。
	s.cfg.IndexNow.SetKey(in.IndexNowKey)
	redirectFlash(w, r, "/admin/site", s.tr(r, "admin.site.saved"))
}
