package web

import (
	"net/http"
	"strings"
)

// handleProfile 显示账号设置：资料与改密。
// handleProfile 把老的资料页并进了站点设置。
//
// 单人站上这一页只剩一个"显示名"加一个改密码按钮，独占一个标签页
// 不值当。保留这个路由是因为它可能存在于书签和旧的跳转里——直接 404
// 会让人以为功能被删了。
func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, langPath(LangFrom(r.Context()), "/admin/site"), http.StatusFound)
}

// handleProfileSave 保存资料。
func (s *Server) handleProfileSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	u := userFrom(r.Context())
	// 单人站上"主页地址"和"简介"这两个框根本没渲染出来，表单里也就没有
	// 这两个字段。直接拿 FormValue 会得到空串，于是保存一次显示名就把
	// 简介抹掉、slug 按新名字重算一遍——旧的 /u/xxx 从此 404，而页面上
	// 没有任何地方提示发生了这件事。
	keep := func(field, cur string) string {
		if r.Form.Has(field) {
			return r.FormValue(field)
		}
		return cur
	}
	if _, err := s.db.UpdateUser(r.Context(), u.ID,
		r.FormValue("name"), keep("bio", u.Bio), keep("slug", u.Slug)); err != nil {
		redirectFlash(w, r, "/admin/site", s.tr(r, "flash.saveFailed", err.Error()))
		return
	}
	redirectFlash(w, r, "/admin/site", s.tr(r, "admin.profile.saved"))
}

// handleChangePassword 修改密码。
//
// 改密会吊销该用户的全部会话（包括当前这个），所以成功后要立刻重新建立
// 本次会话并换发 cookie——否则用户改完密码就被踢回登录页，体验上像是失败了。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	u := userFrom(r.Context())
	next, confirm := r.FormValue("new_password"), r.FormValue("confirm_password")
	if next != confirm {
		redirectFlash(w, r, "/admin/site", s.tr(r, "admin.profile.pwMismatch"))
		return
	}
	if err := s.db.ChangePassword(r.Context(), u.ID, r.FormValue("current_password"), next); err != nil {
		redirectFlash(w, r, "/admin/site", s.tr(r, "flash.opFailed", err.Error()))
		return
	}

	sid, exp, err := s.db.CreateSession(r.Context(), u.ID)
	if err != nil {
		// 密码已经改掉了，只是会话没能重建——如实说明，别让用户以为改失败了
		redirectFlash(w, r, "/login", s.tr(r, "admin.profile.pwRelogin"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sid, Path: "/",
		Expires: exp, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(s.cfg.BaseURL, "https://"),
	})
	redirectFlash(w, r, "/admin/site", s.tr(r, "admin.profile.pwDone"))
}
