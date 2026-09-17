package web

import (
	"net/http"
	"net/url"
	"strings"

	"cligc.com/internal/i18n"
	"cligc.com/internal/store"
)

// handleIndex 是首页：已发布文章的倒序列表。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	pg := pageParam(r)
	lang := LangFrom(r.Context())

	// 精选区只出现在第一页；但精选文章从**所有**分页里排除。
	// 只在第一页排除的话，翻到第二页会看到同一篇再出现一次，而且总数
	// 对不上导致分页错位。模型简单一点：被精选的文章就住在精选区里。
	var featured []store.Post
	if pg == 1 {
		if f, err := s.db.FeaturedPosts(r.Context(), lang.Code, store.MaxFeatured); err == nil {
			featured = f
		}
	}

	posts, total, err := s.db.ListPosts(r.Context(), store.ListFilter{
		Status:          store.StatusPublished,
		Lang:            lang.Code,
		ExcludeFeatured: true,
		Limit:           s.cfg.PerPage,
		Offset:          (pg - 1) * s.cfg.PerPage,
	})
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	prev, next := pager(langPath(lang, "/"), nil, pg, s.cfg.PerPage, total)
	canonical := s.cfg.BaseURL + langPath(lang, "/")
	if pg > 1 {
		canonical = s.cfg.BaseURL + langPath(lang, r.URL.RequestURI())
	}
	s.render(w, r, "index.html", page{
		// 不在这里填 Desc：站点描述可以按语言覆盖，而那个解析发生在 render()
		// 里。直接读 cfg.Description 会绕过覆盖，英文页拿到中文描述。
		// 留空即可，render() 会退回解析后的 SiteDesc。
		// 第 2 页及以后不进索引：分页页面本身没有独立价值，
		// 却会稀释整站的抓取预算。文章本体在 sitemap 里，不会漏收。
		NoIndex:   pg > 1,
		Canonical: canonical,
		Prev:      prev, Next: next,
		Data: map[string]any{
			"Posts": posts, "Total": total, "Page": pg, "Featured": featured,
			"RailTags": s.railTags(r),
		},
	})
}

// handlePost 是文章详情页，站里唯一真正想被收录的页面类型。
func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	p, err := s.db.PostBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.postNotFound"))
		return
	}
	// 草稿只有作者本人和管理员能预览。
	//
	// 归档的文章仍然对所有人可见——归档的用意是"从列表和索引里撤下，
	// 但不让已有链接失效"，把它当 404 处理会制造死链，那是比下架更糟的结果。
	if p.Status == store.StatusDraft {
		u := userFrom(r.Context())
		if u == nil || (!u.IsAdmin() && u.ID != p.UserID) {
			s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.postNotFound"))
			return
		}
	}
	lang, _ := i18n.ByCode(p.Lang)
	// canonical 指向**这一版自己**。把译文的 canonical 指向原文，等于告诉
	// Google 别收录译文——多语种最常见也最致命的错误。
	// 只有站外转载（CanonicalURL）才例外，那是另一回事。
	canonical := s.cfg.BaseURL + langPath(lang, "/p/"+p.Slug)
	if p.CanonicalURL != "" {
		// 站外首发的转载：canonical 指回原站，并且不进索引。
		// 宁可放弃这一篇的收录，也不要让整站被判成采集站。
		canonical = p.CanonicalURL
	}
	// 只有已发布的文章才加载评论：草稿和归档没有公开评论入口，
	// 多一次查询没有意义。
	var comments []store.Comment
	if s.commentsOn(r) && p.IsPublished() {
		if comments, err = s.db.CommentsForPost(r.Context(), p.ID); err != nil {
			// 评论读不出来不该让整篇文章 500，降级成"没有评论"即可
			comments = nil
		}
	}

	others, _ := s.db.Translations(r.Context(), p.TransKey, p.ID)

	s.render(w, r, "post.html", page{
		Title:     p.Title,
		Desc:      p.Summary,
		Canonical: canonical,
		NoIndex:   p.NoIndex(),
		IsArticle: true,
		// 大纲至少要有两条才值得占一栏：只有一条的目录不提供任何导航价值，
		// 却要吃掉整个右侧空间。
		Reading:    len(p.Headings) >= 2,
		Alternates: s.alternatesFor(p, others),
		Flash:      r.URL.Query().Get("flash"),
		Data: map[string]any{
			"Post": p, "Comments": comments,
			"CommentsEnabled": s.commentsOn(r) && p.IsPublished(),
		},
	})
}

// handleTag 是标签页。
func (s *Server) handleTag(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	pg := pageParam(r)
	lang := LangFrom(r.Context())
	posts, total, err := s.db.ListPosts(r.Context(), store.ListFilter{
		Status: store.StatusPublished, TagSlug: tag, Lang: lang.Code,
		Limit: s.cfg.PerPage, Offset: (pg - 1) * s.cfg.PerPage,
	})
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	if total == 0 {
		s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.tagEmpty"))
		return
	}
	prev, next := pager(langPath(lang, "/t/"+url.PathEscape(tag)), nil, pg, s.cfg.PerPage, total)
	s.render(w, r, "list.html", page{
		Title: s.tr(r, "list.tagHeading", tag),
		Desc:  s.tr(r, "list.tagDesc", tag),
		// 标签页一律 noindex：它是站内导航，内容全是别处的摘要，
		// 对搜索引擎是典型的"薄内容"。它仍然对爬虫有用——内链会把
		// 权重传给文章页，所以不加 nofollow，只是不让它自己进索引。
		NoIndex: true,
		Prev:    prev, Next: next,
		Data: map[string]any{"Posts": posts, "Total": total, "Page": pg,
			"Heading": s.tr(r, "list.tagHeading", tag), "RailTags": s.railTags(r)},
	})
}

// handleAuthor 是作者页。
func (s *Server) handleAuthor(w http.ResponseWriter, r *http.Request) {
	u, err := s.db.UserBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.authorNotFound"))
		return
	}
	pg := pageParam(r)
	lang := LangFrom(r.Context())
	posts, total, err := s.db.ListPosts(r.Context(), store.ListFilter{
		Status: store.StatusPublished, UserID: u.ID, Lang: lang.Code,
		Limit: s.cfg.PerPage, Offset: (pg - 1) * s.cfg.PerPage,
	})
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	prev, next := pager(langPath(lang, "/u/"+url.PathEscape(u.Slug)), nil, pg, s.cfg.PerPage, total)
	multiAuthor := false
	if n, err := s.db.CountUsers(r.Context()); err == nil {
		multiAuthor = n > 1
	}
	s.render(w, r, "list.html", page{
		Title: u.Name,
		Desc:  s.tr(r, "list.authorDesc", u.Name),
		// 空作者页是纯粹的垃圾索引项；有内容的作者页仍然是聚合页，
		// 只在第一页允许收录。
		// 单人站上这一页和首页列的是同一批文章，进索引就是重复内容。
		// 路由保留（外链不断），但不让搜索引擎收。
		NoIndex: total == 0 || pg > 1 || !multiAuthor,
		Prev:    prev, Next: next,
		Data: map[string]any{"Posts": posts, "Total": total, "Page": pg,
			"Heading": u.Name, "Bio": u.Bio, "RailTags": s.railTags(r)},
	})
}

// handleSearch 是站内检索页。
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	pg := pageParam(r)
	data := map[string]any{"Query": q, "Page": pg, "RailTags": s.railTags(r)}

	if q != "" {
		hits, total, err := s.db.Search(r.Context(), q, store.SearchFilter{
			Status: store.StatusPublished, Lang: LangFrom(r.Context()).Code,
			Limit: s.cfg.PerPage, Offset: (pg - 1) * s.cfg.PerPage,
		})
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
			return
		}
		data["Hits"] = hits
		data["Total"] = total
		prev, next := pager(langPath(LangFrom(r.Context()), "/search"),
			url.Values{"q": {q}}, pg, s.cfg.PerPage, total)
		s.render(w, r, "search.html", page{
			Title: s.tr(r, "search.titleFmt", q), NoIndex: true, Prev: prev, Next: next, Data: data,
		})
		return
	}
	// 站内搜索结果页永远 noindex：它由用户输入生成，数量无限，
	// 是搜索引擎明确点名的低质量页面类型。
	s.render(w, r, "search.html", page{Title: s.tr(r, "search.title"), NoIndex: true, Data: data})
}

// handleNotFound 是兜底路由，把未匹配的路径渲染成站点自己的 404 页。
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.notFound"))
}

// handleHealth 供负载均衡和部署脚本探活。
//
// 它真的去 ping 一次数据库：只回 "ok" 而不碰 DB 的健康检查，
// 会在磁盘满或文件被误删时依然报健康，那种检查没有意义。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := s.db.R.PingContext(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// railTags 取侧栏用的标签。
//
// 上限 24 个：侧栏是导航不是目录，一屏放不下的标签云只会变成噪音。
// 取不到就返回 nil，模板据此退回单栏居中——侧栏空着比留一块空白好。
func (s *Server) railTags(r *http.Request) []store.Tag {
	tags, err := s.db.ListTags(r.Context())
	if err != nil || len(tags) == 0 {
		return nil
	}
	if len(tags) > 24 {
		tags = tags[:24]
	}
	return tags
}
