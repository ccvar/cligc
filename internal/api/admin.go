// 后台能做的其余操作的 API 映射：精选、归档、定时、评论审核、媒体删除、
// token 管理、站点设置、作者资料。
//
// 唯一没有对应接口的是改密码——密码是找回账号的最后一个锚点。它一旦能被
// 程序改掉，token 泄露就从"内容被乱动"升级成"账号彻底拿不回来"。
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cligc.com/internal/store"
)

// --- 文章：归档 / 精选 / 定时 ---

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "post")
	if !ok {
		return
	}
	p, err := s.db.SetStatus(r.Context(), actor(r.Context()), id, store.StatusArchived)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.dto(p, false))
}

func (s *Server) handleFeature(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "post")
	if !ok {
		return
	}
	var in struct {
		Featured *bool `json:"featured"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	on := in.Featured == nil || *in.Featured
	p, err := s.db.SetFeatured(r.Context(), actor(r.Context()), id, on)
	if err != nil {
		fail(w, err)
		return
	}
	n, _ := s.db.CountFeatured(r.Context())
	body := map[string]any{"post": s.dto(p, false), "featured_total": n}
	if n > store.MaxFeatured {
		body["note"] = "Only the " + strconv.Itoa(store.MaxFeatured) +
			" most recently featured posts appear on the home page."
	}
	writeJSON(w, http.StatusOK, body)
}

// handleSchedule 设置或取消定时发布。
//
// 定时走的是 posts:publish 而不是 posts:write：它就是发布，只是晚一点。
// 归到写权限里的话，"AI 只能写草稿"这句话在一天之后就不成立了。
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "post")
	if !ok {
		return
	}
	var in struct {
		PublishAt string `json:"publish_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "body must be JSON")
		return
	}
	var at *time.Time
	if v := strings.TrimSpace(in.PublishAt); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			badRequest(w, "publish_at must be RFC 3339, e.g. 2026-01-02T15:04:05+08:00, or empty to cancel")
			return
		}
		at = &t
	}
	if err := s.db.SetSchedule(r.Context(), actor(r.Context()), id, at); err != nil {
		fail(w, err)
		return
	}
	p, err := s.db.PostByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	body := map[string]any{"post": s.dto(p, false)}
	if at == nil {
		body["note"] = "Schedule cleared. The post stays a draft until published."
	} else {
		body["note"] = "Stays a draft — invisible to listings, search, sitemap and feed — until it goes live."
	}
	writeJSON(w, http.StatusOK, body)
}

// --- 评论审核 ---

func (s *Server) handleCommentStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r, "comment")
		if !ok {
			return
		}
		if err := s.db.SetCommentStatus(r.Context(), actor(r.Context()), id, status); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
	}
}

func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "comment")
	if !ok {
		return
	}
	if err := s.db.DeleteComment(r.Context(), actor(r.Context()), id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- 媒体删除 ---

func (s *Server) handleDeleteMedia(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "media")
	if !ok {
		return
	}
	if err := s.db.DeleteMedia(r.Context(), actor(r.Context()), s.cfg.MediaRoot, id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- token 管理 ---

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.db.ListTokens(r.Context(), userFrom(r.Context()).ID)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "prefix": t.Prefix, "scopes": t.Scopes,
			"created_at": t.CreatedAt, "last_used_at": t.LastUsedAt,
			"expires_at": t.ExpiresAt, "revoked_at": t.RevokedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
		Days   int      `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "body must be JSON")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		badRequest(w, "name is required — it is how you tell tokens apart when revoking one")
		return
	}
	var ttl time.Duration
	if in.Days > 0 {
		ttl = time.Duration(in.Days) * 24 * time.Hour
	}
	// 传当前 token 自己的 scopes 进去：签发出来的不得超出它自身的权限。
	// 少了这一条，一个只有 posts:write 的 token 可以给自己签一个带
	// posts:publish 的，"AI 只能写草稿"这道闸门一次调用就绕过去了。
	parent := tokenFrom(r.Context())
	var parentScopes []string
	if parent != nil {
		parentScopes = parent.Scopes
	}
	plain, t, err := s.db.CreateTokenAs(r.Context(), parentScopes,
		userFrom(r.Context()).ID, in.Name, in.Scopes, ttl)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": t.ID, "name": t.Name, "prefix": t.Prefix, "scopes": t.Scopes,
		"token": plain,
		"note":  "Shown once. Only its SHA-256 is stored.",
	})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "token")
	if !ok {
		return
	}
	if err := s.db.RevokeToken(r.Context(), userFrom(r.Context()).ID, id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- 站点设置 ---

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, siteDTO(s.db.Settings(r.Context())))
}

func (s *Server) handlePatchSite(w http.ResponseWriter, r *http.Request) {
	cur := s.db.Settings(r.Context())
	var in struct {
		SiteTitle       *string `json:"site_title"`
		SiteDescription *string `json:"site_description"`
		CommentsEnabled *bool   `json:"comments_enabled"`
		GoogleVerify    *string `json:"google_verify"`
		BingVerify      *string `json:"bing_verify"`
		GA4ID           *string `json:"ga4_id"`
		IndexNowKey     *string `json:"indexnow_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "body must be JSON")
		return
	}
	// 指针语义：没给的字段保持不变，给了空串才是清空。全量覆盖的话，
	// 一次只想改 GA4 的调用会把另外三项一并抹掉。
	if in.SiteTitle != nil {
		cur.SiteTitle = *in.SiteTitle
	}
	if in.SiteDescription != nil {
		cur.SiteDescription = *in.SiteDescription
	}
	if in.CommentsEnabled != nil {
		cur.CommentsEnabled = *in.CommentsEnabled
	}
	if in.GoogleVerify != nil {
		cur.GoogleVerify = *in.GoogleVerify
	}
	if in.BingVerify != nil {
		cur.BingVerify = *in.BingVerify
	}
	if in.GA4ID != nil {
		cur.GA4ID = *in.GA4ID
	}
	if in.IndexNowKey != nil {
		cur.IndexNowKey = *in.IndexNowKey
	}
	if err := s.db.SaveSettings(r.Context(), cur); err != nil {
		fail(w, err)
		return
	}
	if s.cfg.OnSettingsSaved != nil {
		s.cfg.OnSettingsSaved(cur)
	}
	writeJSON(w, http.StatusOK, siteDTO(cur))
}

func siteDTO(st store.SiteSettings) map[string]any {
	return map[string]any{
		"site_title":       st.SiteTitle,
		"site_description": st.SiteDescription,
		"comments_enabled": st.CommentsEnabled,
		"google_verify":    st.GoogleVerify,
		"bing_verify":      st.BingVerify,
		"ga4_id":           st.GA4ID,
		"indexnow_key":     st.IndexNowKey,
		"notes": map[string]string{
			"ga4_id":       "Setting this loads a third-party script and opens script-src to googletagmanager.com. Leave empty for no third-party requests.",
			"indexnow_key": "8-128 hex chars. Notifies Bing/Yandex/Seznam/Naver on every change. Google does not support IndexNow.",
		},
	}
}

// --- 作者资料 ---

func (s *Server) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var in struct {
		Name *string `json:"name"`
		Slug *string `json:"slug"`
		Bio  *string `json:"bio"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "body must be JSON")
		return
	}
	name, slug, bio := u.Name, u.Slug, u.Bio
	if in.Name != nil {
		name = *in.Name
	}
	if in.Slug != nil {
		slug = *in.Slug
	}
	if in.Bio != nil {
		bio = *in.Bio
	}
	got, err := s.db.UpdateUser(r.Context(), u.ID, name, bio, slug)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": got.ID, "name": got.Name, "slug": got.Slug, "bio": got.Bio,
		"note": "Email and password cannot be changed through the API.",
	})
}

// pathID 读出并校验路径里的数字 ID。
func pathID(w http.ResponseWriter, r *http.Request, what string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "%s id must be numeric", what)
		return 0, false
	}
	return id, true
}

// --- 分类 ---

func (s *Server) handleListCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := s.db.ListCategories(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(cats))
	for _, c := range cats {
		out = append(out, map[string]any{
			"id": c.ID, "slug": c.Slug, "name": c.Name, "description": c.Desc,
			"sort": c.Sort, "published": c.Count,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": out,
		"note": "Categories are the site's sections: few, stable, one per post. " +
			"Tags are the other dimension — many, flat, several per post. Don't mix them.",
	})
}

func (s *Server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
		Desc string `json:"description"`
		Sort int    `json:"sort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "body must be JSON")
		return
	}
	c, err := s.db.CreateCategory(r.Context(), in.Name, in.Slug, in.Desc, in.Sort)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": c.ID, "slug": c.Slug, "name": c.Name, "description": c.Desc, "sort": c.Sort})
}

func (s *Server) handleUpdateCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "category")
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
		Desc string `json:"description"`
		Sort int    `json:"sort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "body must be JSON")
		return
	}
	if err := s.db.UpdateCategory(r.Context(), id, in.Name, in.Slug, in.Desc, in.Sort); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": in.Name, "slug": in.Slug,
		"description": in.Desc, "sort": in.Sort})
}

func (s *Server) handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "category")
	if !ok {
		return
	}
	if err := s.db.DeleteCategory(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	// 说清楚后果：删分类是整理导航，不是删内容。AI 客户端拿到这句话
	// 才知道不必向用户报告"文章丢了"。
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": id,
		"note":    "Posts in this category are now uncategorised. None were deleted.",
	})
}
