// Package api 实现 /api/v1 下的 REST 接口。
//
// 这一层是全站唯一的写入口：网页后台、MCP server 都走它。刻意不给 AI 单独
// 开一套接口——两套写路径意味着两套校验、两套权限判断，迟早有一套落后于另一套。
//
// 面向 AI 客户端的接口设计和面向浏览器的不太一样，有三条额外约束贯穿本文件：
//
//  1. 返回体要省 context。列表只给标题和摘要，正文必须显式索取。一个
//     5000 字的正文就是几千 token，AI 读三篇就把上下文挤满了。
//  2. 写操作要幂等。AI 客户端会重试，Idempotency-Key 是必选项不是加分项。
//  3. 发布是独立的权限。posts:write 只能写草稿，发布要 posts:publish。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cligc.com/internal/store"
)

// Config 是 API 层需要的运行期设置。
type Config struct {
	BaseURL         string // 用于拼出文章的绝对 URL，如 https://cligc.com
	MediaRoot       string // 上传文件落盘根目录
	DailyPublishCap int    // 每用户每日发布上限，<=0 表示不限

	// OnSettingsSaved 在站点设置通过 API 改动后被调用。
	//
	// 需要它是因为有些设置在进程里有活的副本——IndexNow 的 key 就在
	// 提交器的内存里。只写库不通知的话，改完 key 之后下一次发布还在用
	// 旧的，而且没有任何报错，表现是"提交了但一直 403"。
	OnSettingsSaved func(store.SiteSettings)
}

// Server 持有 API 的依赖。
type Server struct {
	db  *store.DB
	cfg Config
	lim *limiter
}

// New 构造 API server。
func New(db *store.DB, cfg Config) *Server {
	return &Server{db: db, cfg: cfg, lim: newLimiter(60, time.Minute)}
}

// Routes 把 API 路由注册到 mux 上。用 Go 1.22 起的方法+通配符模式，
// 不需要第三方路由库。
func (s *Server) Routes(mux *http.ServeMux) {
	h := func(scope string, fn http.HandlerFunc) http.Handler {
		return s.auth(scope, fn)
	}
	mux.Handle("GET /api/v1/me", h(store.ScopePostsRead, s.handleMe))

	mux.Handle("GET /api/v1/posts", h(store.ScopePostsRead, s.handleListPosts))
	mux.Handle("GET /api/v1/posts/{id}", h(store.ScopePostsRead, s.handleGetPost))
	mux.Handle("POST /api/v1/posts", h(store.ScopePostsWrite, s.handleCreatePost))
	mux.Handle("PATCH /api/v1/posts/{id}", h(store.ScopePostsWrite, s.handleUpdatePost))
	mux.Handle("DELETE /api/v1/posts/{id}", h(store.ScopePostsWrite, s.handleDeletePost))
	mux.Handle("POST /api/v1/posts/{id}/publish", h(store.ScopePostsPublish, s.handlePublish))
	mux.Handle("POST /api/v1/posts/{id}/unpublish", h(store.ScopePostsPublish, s.handleUnpublish))

	// 归档、精选、定时都算"改变对外可见性"，一律归在 posts:publish 下。
	// 定时尤其不能算写权限——它就是发布，只是晚一点；归到 posts:write 里，
	// "AI 只能写草稿"这句话在一天之后就不成立了。
	mux.Handle("POST /api/v1/posts/{id}/archive", h(store.ScopePostsPublish, s.handleArchive))
	mux.Handle("POST /api/v1/posts/{id}/feature", h(store.ScopePostsPublish, s.handleFeature))
	mux.Handle("POST /api/v1/posts/{id}/schedule", h(store.ScopePostsPublish, s.handleSchedule))

	mux.Handle("GET /api/v1/tags", h(store.ScopePostsRead, s.handleTags))

	// 分类走 posts:write，和标签同级——它们都是内容组织，不是站点设置。
	// 删分类不会删文章（外键是 on delete set null），所以不需要更高的权限。
	mux.Handle("GET /api/v1/categories", h(store.ScopePostsRead, s.handleListCategories))
	mux.Handle("POST /api/v1/categories", h(store.ScopePostsWrite, s.handleCreateCategory))
	mux.Handle("PATCH /api/v1/categories/{id}", h(store.ScopePostsWrite, s.handleUpdateCategory))
	mux.Handle("DELETE /api/v1/categories/{id}", h(store.ScopePostsWrite, s.handleDeleteCategory))

	// 评论。标垃圾留在 posts:write 下（那是"把可疑内容藏起来"，方向安全）；
	// 通过审核、退回、删除要单独的 comments:moderate——见那个 scope 的注释。
	mux.Handle("GET /api/v1/comments", h(store.ScopePostsRead, s.handleListComments))
	mux.Handle("POST /api/v1/comments/{id}/spam", h(store.ScopePostsWrite, s.handleFlagSpam))
	mux.Handle("POST /api/v1/comments/{id}/approve",
		h(store.ScopeCommentsModerate, s.handleCommentStatus(store.CommentApproved)))
	mux.Handle("POST /api/v1/comments/{id}/pending",
		h(store.ScopeCommentsModerate, s.handleCommentStatus(store.CommentPending)))
	mux.Handle("DELETE /api/v1/comments/{id}", h(store.ScopeCommentsModerate, s.handleDeleteComment))

	mux.Handle("GET /api/v1/media", h(store.ScopePostsRead, s.handleListMedia))
	mux.Handle("POST /api/v1/media", h(store.ScopeMediaWrite, s.handleUploadMedia))
	mux.Handle("DELETE /api/v1/media/{id}", h(store.ScopeMediaDelete, s.handleDeleteMedia))

	mux.Handle("GET /api/v1/tokens", h(store.ScopeTokensManage, s.handleListTokens))
	mux.Handle("POST /api/v1/tokens", h(store.ScopeTokensManage, s.handleCreateToken))
	mux.Handle("POST /api/v1/tokens/{id}/revoke", h(store.ScopeTokensManage, s.handleRevokeToken))

	mux.Handle("GET /api/v1/site", h(store.ScopeSiteAdmin, s.handleGetSite))
	mux.Handle("PATCH /api/v1/site", h(store.ScopeSiteAdmin, s.handlePatchSite))
	// 改密码刻意没有接口：密码是找回账号的最后一个锚点。
	mux.Handle("PATCH /api/v1/me", h(store.ScopeSiteAdmin, s.handlePatchMe))
}

// --- 响应工具 ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// errorBody 是统一的错误响应。code 是稳定的机器可读串，
// MCP 层会把它原样透给 AI —— 比 HTTP 状态码更好让模型自我纠正。
type errorBody struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Details string `json:"details,omitempty"`
}

// fail 把领域错误映射成 HTTP 状态码。上层一律用 errors.Is 判断，
// 不做字符串匹配。
func fail(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal_error"
	switch {
	case errors.Is(err, store.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, store.ErrForbidden):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, store.ErrUnauthorized):
		status, code = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, store.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, store.ErrInvalidInput):
		status, code = http.StatusBadRequest, "invalid_input"
	case errors.Is(err, store.ErrDailyCap):
		status, code = http.StatusTooManyRequests, "daily_publish_cap"
	}
	msg := err.Error()
	if status == http.StatusInternalServerError {
		msg = "internal error" // 不把内部错误细节透给客户端
	}
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

func badRequest(w http.ResponseWriter, format string, args ...any) {
	writeJSON(w, http.StatusBadRequest, errorBody{
		Error: fmt.Sprintf(format, args...), Code: "invalid_input"})
}

// decode 读取并解析 JSON 请求体，带大小上限。
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20) // 2 MiB
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // 拼错的字段名要报错，不要静默忽略
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s", store.ErrInvalidInput, err.Error())
	}
	return nil
}

func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func boolParam(r *http.Request, name string) bool {
	v := strings.ToLower(r.URL.Query().Get(name))
	return v == "1" || v == "true" || v == "yes"
}
