package web

import (
	"net/http"
	"net/url"
	"strconv"

	"cligc.com/internal/store"
)

// handleCategory 是公开的板块页：/c/{slug}
//
// 和标签页一样 noindex,follow —— 它仍然是一个薄聚合页，独立价值来自
// 底下的文章而不是这一页本身。分类的价值在导航，不在被收录：
// 让读者知道"这个站分几块"，而不是让搜索引擎多抓几个列表页。
func (s *Server) handleCategory(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	c, err := s.db.CategoryBySlug(r.Context(), slug)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.notFound"))
		return
	}
	lang := LangFrom(r.Context())
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
		Title:   s.tr(r, "list.catHeading", c.Name),
		Desc:    s.tr(r, "list.catDesc", c.Name),
		NoIndex: true,
		Prev:    prev, Next: next,
		Data: map[string]any{
			"Heading": s.tr(r, "list.catHeading", c.Name),
			"Posts":   posts, "Total": total,
		},
	})
}

// --- 后台 ---

func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := s.db.ListCategories(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	s.render(w, r, "admin_categories.html", page{
		Title: s.tr(r, "admin.cats.title"), NoIndex: true, Wide: true, AdminTab: "categories",
		Flash: r.URL.Query().Get("flash"),
		Data:  map[string]any{"Cats": cats},
	})
}

func (s *Server) handleCategorySave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	sort, _ := strconv.Atoi(r.FormValue("sort"))
	name, slug := r.FormValue("name"), r.FormValue("slug")

	var err error
	if id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64); id > 0 {
		err = s.db.UpdateCategory(r.Context(), id, name, slug, sort)
	} else {
		_, err = s.db.CreateCategory(r.Context(), name, slug, sort)
	}
	if err != nil {
		redirectFlash(w, r, "/admin/categories", s.tr(r, "flash.saveFailed", err.Error()))
		return
	}
	redirectFlash(w, r, "/admin/categories", s.tr(r, "admin.cats.saved"))
}

func (s *Server) handleCategoryDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badID"))
		return
	}
	if err := s.db.DeleteCategory(r.Context(), id); err != nil {
		redirectFlash(w, r, "/admin/categories", s.tr(r, "flash.opFailed", err.Error()))
		return
	}
	redirectFlash(w, r, "/admin/categories", s.tr(r, "admin.cats.deleted"))
}
