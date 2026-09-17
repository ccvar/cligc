package web

import (
	"net/http"
	"net/url"
	"strconv"

	"cligc.com/internal/i18n"
	"cligc.com/internal/store"
)

// handleCategory 是公开的板块页：/c/{slug}
//
// 和标签页一样 noindex,follow —— 它仍然是一个薄聚合页，独立价值来自
// 底下的文章而不是这一页本身。分类的价值在导航，不在被收录：
// 让读者知道"这个站分几块"，而不是让搜索引擎多抓几个列表页。
func (s *Server) handleCategory(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	lang := LangFrom(r.Context())
	// 板块名按当前语言解析：一个叫「随笔」的板块，在英文站上标题写着
	// 「随笔」，那一页的 <title> 和面包屑就都是没人看得懂的。
	c, err := s.db.CategoryBySlug(r.Context(), slug, lang.Code)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.notFound"))
		return
	}
	pg := pageParam(r)
	posts, total, err := s.db.ListPosts(r.Context(), store.ListFilter{
		Status: store.StatusPublished, CategorySlug: c.Slug, Lang: lang.Code,
		Limit: s.cfg.PerPage, Offset: (pg - 1) * s.cfg.PerPage,
	})
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	prev, next := pager(langPath(lang, "/c/"+url.PathEscape(c.Slug)), nil, pg, s.cfg.PerPage, total)
	s.render(w, r, "list.html", page{
		Title: s.tr(r, "list.catHeading", c.Name),
		// 有描述就用描述，没有才退回"XX 板块下的文章"。板块页是聚合页，
		// 没有正文可以自动截取，这一句就是搜索结果里显示的全部。
		Desc:    firstNonEmpty(c.Desc, s.tr(r, "list.catDesc", c.Name)),
		NoIndex: true,
		Prev:    prev, Next: next,
		Data: map[string]any{
			"Heading": s.tr(r, "list.catHeading", c.Name),
			"Lede":    c.Desc,
			"Posts":   posts, "Total": total,
		},
	})
}

// --- 后台 ---

// handleCategories 把老的分类页并进了设置。保留这个路由是因为它
// 可能存在于书签里——直接 404 会让人以为功能被删了。
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, langPath(LangFrom(r.Context()), "/admin/site"), http.StatusFound)
}

func (s *Server) handleCategorySave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	// 排序那一列从表格里拿掉了（顺序靠拖），所以行表单不带 sort——
	// 这时传 nil，让 store 别动这一列。
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	var sort *int
	if r.Form.Has("sort") {
		n, _ := strconv.Atoi(r.FormValue("sort"))
		sort = &n
	}
	in := store.CategoryInput{
		Slug: r.FormValue("slug"), Sort: sort, DefaultLang: i18n.Default().Code,
		Names: map[string]string{}, Descs: map[string]string{},
	}
	// 表单里按语言的字段叫 name:en / description:en。只认对外提供的
	// 那几种语言——多出来的键是客户端瞎填的，不该进库。
	for _, l := range s.enabledLangs(r.Context()) {
		in.Names[l.Code] = r.FormValue("name:" + l.Code)
		in.Descs[l.Code] = r.FormValue("description:" + l.Code)
	}
	// 只有一种语言时表单不带前缀，走简写字段。
	if v := r.FormValue("name"); v != "" {
		in.Names[in.DefaultLang] = v
	}
	if v := r.FormValue("description"); v != "" {
		in.Descs[in.DefaultLang] = v
	}

	var err error
	if id > 0 {
		err = s.db.UpdateCategory(r.Context(), id, in)
	} else {
		_, err = s.db.CreateCategory(r.Context(), in)
	}
	if err != nil {
		redirectFlash(w, r, "/admin/site", s.tr(r, "flash.saveFailed", err.Error()))
		return
	}
	redirectFlash(w, r, "/admin/site", s.tr(r, "admin.cats.saved"))
}

func (s *Server) handleCategoryDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badID"))
		return
	}
	if err := s.db.DeleteCategory(r.Context(), id); err != nil {
		redirectFlash(w, r, "/admin/site", s.tr(r, "flash.opFailed", err.Error()))
		return
	}
	redirectFlash(w, r, "/admin/site", s.tr(r, "admin.cats.deleted"))
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// handleCategoryReorder 接收拖动排序的结果：一串按新顺序排列的 ID。
//
// 单独一条路由而不是混进 handleCategorySave：那个是"改一个分类的字段"，
// 这个是"改所有分类的相对位置"，一次请求动的行数不一样，失败的后果也不一样。
func (s *Server) handleCategoryReorder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	raw := r.Form["id"]
	ids := make([]int64, 0, len(raw))
	for _, v := range raw {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badID"))
			return
		}
		ids = append(ids, n)
	}
	if len(ids) == 0 {
		redirectFlash(w, r, "/admin/site", "")
		return
	}
	if err := s.db.ReorderCategories(r.Context(), ids); err != nil {
		redirectFlash(w, r, "/admin/site", s.tr(r, "flash.opFailed", err.Error()))
		return
	}
	redirectFlash(w, r, "/admin/site", s.tr(r, "admin.cats.saved"))
}
