package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cligc.com/internal/store"
)

// postDTO 是文章的对外表示。
//
// 正文（BodyMD）默认不出现——列表和详情的默认响应都只给标题与摘要。
// 这是专门为 AI 客户端做的取舍：一篇 5000 字的文章就是几千 token，
// 列 20 篇就能把上下文占满，而绝大多数调用其实只需要知道"有哪些文章"。
// 要正文得显式传 ?full=true。
type postDTO struct {
	ID          int64      `json:"id"`
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	Summary     string     `json:"summary"`
	Status      string     `json:"status"`
	Source      string     `json:"source"`
	Lang        string     `json:"lang"`
	Category    string     `json:"category,omitempty"`
	Tags        []string   `json:"tags"`
	WordCount   int        `json:"word_count"`
	URL         string     `json:"url"`
	Author      string     `json:"author"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`

	// 以下字段仅在 full=true 时填充
	BodyMD       string `json:"body_md,omitempty"`
	Indexable    *bool  `json:"indexable,omitempty"`
	CanonicalURL string `json:"canonical_url,omitempty"`
	NoIndex      *bool  `json:"noindex,omitempty"`
}

func (s *Server) dto(p *store.Post, full bool) postDTO {
	tags := make([]string, 0, len(p.Tags))
	for _, t := range p.Tags {
		tags = append(tags, t.Name)
	}
	d := postDTO{
		ID: p.ID, Slug: p.Slug, Title: p.Title, Summary: p.Summary,
		Status: p.Status, Source: p.Source, Lang: p.Lang, Category: p.CategorySlug,
		Tags: tags, WordCount: p.WordCount,
		URL:       strings.TrimRight(s.cfg.BaseURL, "/") + "/p/" + p.Slug,
		Author:    p.AuthorName,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, PublishedAt: p.PublishedAt,
	}
	if full {
		ni := p.NoIndex()
		d.BodyMD = p.BodyMD
		d.Indexable = &p.Indexable
		d.CanonicalURL = p.CanonicalURL
		d.NoIndex = &ni
	}
	return d
}

// handleMe 返回当前身份和这枚 token 能做什么。
//
// AI 客户端在动手前先调它，就能知道自己有没有发布权、今天还剩多少配额，
// 从而不去尝试注定被拒的操作。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, tok := userFrom(r.Context()), tokenFrom(r.Context())
	used, err := s.db.PublishedToday(r.Context(), u.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":              map[string]any{"id": u.ID, "name": u.Name, "slug": u.Slug, "role": u.Role},
		"token":             map[string]any{"name": tok.Name, "scopes": tok.Scopes},
		"can_publish":       tok.Has(store.ScopePostsPublish),
		"published_today":   used,
		"daily_publish_cap": s.cfg.DailyPublishCap,
		"publish_remaining": remaining(s.cfg.DailyPublishCap, used),
		"base_url":          strings.TrimRight(s.cfg.BaseURL, "/"),
	})
}

func remaining(cap, used int) any {
	if cap <= 0 {
		return nil // null = 不限
	}
	if n := cap - used; n > 0 {
		return n
	}
	return 0
}

// handleListPosts 列出文章。
//
// scope 默认为 mine：只返回 token 所属用户的文章。这一条是安全边界——
// 默认全站会让别人的草稿从这个接口漏出去。要看全站内容得显式传 scope=site，
// 且只返回已发布的。
func (s *Server) handleListPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	limit, offset := intParam(r, "limit", 20), intParam(r, "offset", 0)

	siteWide := r.URL.Query().Get("scope") == "site"
	status := r.URL.Query().Get("status")
	var userID int64
	if siteWide {
		status = store.StatusPublished // 全站范围只暴露已发布内容
	} else {
		userID = u.ID
	}

	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		hits, total, err := s.db.Search(ctx, q, store.SearchFilter{
			UserID: userID, Status: status, Limit: limit, Offset: offset,
		})
		if err != nil {
			fail(w, err)
			return
		}
		items := make([]map[string]any, 0, len(hits))
		for _, h := range hits {
			items = append(items, map[string]any{
				"post": s.dto(&h.Post, false), "snippet": h.Snippet,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"results": items, "total": total, "limit": limit, "offset": offset, "query": q})
		return
	}

	posts, total, err := s.db.ListPosts(ctx, store.ListFilter{
		Status: status, UserID: userID, TagSlug: r.URL.Query().Get("tag"),
		// 语言过滤是可选的：AI 客户端常常需要跨语言找"这篇的原文在哪"，
		// 默认就不该限制。
		Lang:         r.URL.Query().Get("lang"),
		CategorySlug: r.URL.Query().Get("category"),
		Limit:        limit, Offset: offset,
	})
	if err != nil {
		fail(w, err)
		return
	}
	items := make([]postDTO, 0, len(posts))
	for i := range posts {
		items = append(items, s.dto(&posts[i], false))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"posts": items, "total": total, "limit": limit, "offset": offset})
}

// lookup 按数字 id 或 slug 取文章，并检查读权限：
// 未发布的内容只有作者本人和管理员看得到。
func (s *Server) lookup(r *http.Request) (*store.Post, error) {
	ctx := r.Context()
	key := r.PathValue("id")
	var (
		p   *store.Post
		err error
	)
	if id, e := strconv.ParseInt(key, 10, 64); e == nil {
		p, err = s.db.PostByID(ctx, id)
	} else {
		p, err = s.db.PostBySlug(ctx, key)
	}
	if err != nil {
		return nil, err
	}
	u := userFrom(ctx)
	if !p.IsPublished() && !(u.IsAdmin() || p.UserID == u.ID) {
		// 对无权者伪装成不存在，不泄露"这个 slug 有一篇草稿"
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (s *Server) handleGetPost(w http.ResponseWriter, r *http.Request) {
	p, err := s.lookup(r)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.dto(p, boolParam(r, "full")))
}

type createReq struct {
	Title          string   `json:"title"`
	BodyMD         string   `json:"body_md"`
	Slug           string   `json:"slug"`
	Summary        string   `json:"summary"`
	Tags           []string `json:"tags"`
	Source         string   `json:"source"`
	CanonicalURL   string   `json:"canonical_url"`
	Indexable      *bool    `json:"indexable"`
	Lang           string   `json:"lang"`
	Category       string   `json:"category"` // 分类 slug，留空表示未分类
	TranslationOf  int64    `json:"translation_of"`
	IdempotencyKey string   `json:"idempotency_key"`
}

// handleCreatePost 新建草稿。注意：永远是草稿，这个接口不能直接发布。
func (s *Server) handleCreatePost(w http.ResponseWriter, r *http.Request) {
	var in createReq
	if err := decode(w, r, &in); err != nil {
		fail(w, err)
		return
	}
	// 也接受 Idempotency-Key 请求头，方便通用 HTTP 客户端
	key := in.IdempotencyKey
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}

	p, err := s.db.CreatePost(r.Context(), actor(r.Context()), store.CreatePostInput{
		Title: in.Title, BodyMD: in.BodyMD, Slug: in.Slug, Summary: in.Summary,
		Tags: in.Tags, Source: in.Source, CanonicalURL: in.CanonicalURL,
		Indexable: in.Indexable, Lang: in.Lang, CategorySlug: in.Category,
		IdempotencyKey: key,
	})
	if err != nil {
		fail(w, err)
		return
	}
	// 关联译文分组。失败不回滚草稿——草稿已经建好了，把它删掉换来的只是
	// 一个"什么都没发生"的假象，而作者的内容就没了。
	if in.TranslationOf != 0 {
		if err := s.db.LinkTranslation(r.Context(), actor(r.Context()), p.ID, in.TranslationOf); err != nil {
			writeJSON(w, http.StatusCreated, map[string]any{
				"post": s.dto(p, true),
				"warning": "Draft created, but linking it as a translation failed: " + err.Error() +
					". Fix the link in the admin UI; the draft itself is safe.",
			})
			return
		}
		p, _ = s.db.PostByID(r.Context(), p.ID)
	}
	writeJSON(w, http.StatusCreated, s.dto(p, true))
}

type updateReq struct {
	Title        *string   `json:"title"`
	BodyMD       *string   `json:"body_md"`
	Slug         *string   `json:"slug"`
	Summary      *string   `json:"summary"`
	Tags         *[]string `json:"tags"`
	Source       *string   `json:"source"`
	CanonicalURL *string   `json:"canonical_url"`
	Indexable    *bool     `json:"indexable"`
	Lang         *string   `json:"lang"`
	Category     *string   `json:"category"` // 空串取消分类，缺省不改
}

func (s *Server) handleUpdatePost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "post id must be numeric for updates")
		return
	}
	var in updateReq
	if err := decode(w, r, &in); err != nil {
		fail(w, err)
		return
	}
	p, err := s.db.UpdatePost(r.Context(), actor(r.Context()), id, store.UpdatePostInput{
		Title: in.Title, BodyMD: in.BodyMD, Slug: in.Slug, Summary: in.Summary,
		Tags: in.Tags, Source: in.Source, CanonicalURL: in.CanonicalURL,
		Indexable: in.Indexable, Lang: in.Lang, CategorySlug: in.Category,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.dto(p, true))
}

// handlePublish 发布一篇草稿。需要 posts:publish scope（默认不发给 AI）。
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "post id must be numeric")
		return
	}
	p, err := s.db.PublishPost(r.Context(), actor(r.Context()), id, s.cfg.DailyPublishCap)
	if err != nil {
		if errors.Is(err, store.ErrDailyCap) {
			used, _ := s.db.PublishedToday(r.Context(), userFrom(r.Context()).ID)
			writeJSON(w, http.StatusTooManyRequests, errorBody{
				Error: "daily publish cap reached", Code: "daily_publish_cap",
				Details: "published today: " + strconv.Itoa(used) +
					"; cap: " + strconv.Itoa(s.cfg.DailyPublishCap) +
					". The post stays a draft and can be published tomorrow or from the admin UI.",
			})
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.dto(p, true))
}

func (s *Server) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "post id must be numeric")
		return
	}
	p, err := s.db.UnpublishPost(r.Context(), actor(r.Context()), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.dto(p, true))
}

func (s *Server) handleDeletePost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "post id must be numeric")
		return
	}
	if err := s.db.DeletePost(r.Context(), actor(r.Context()), id); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.db.ListTags(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(tags))
	for _, t := range tags {
		out = append(out, map[string]any{"name": t.Name, "slug": t.Slug, "count": t.Count})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": out})
}

// --- 媒体 ---

func (s *Server) handleListMedia(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.ListMedia(r.Context(), userFrom(r.Context()).ID, intParam(r, "limit", 50))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, m := range items {
		out = append(out, s.mediaDTO(&m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"media": out})
}

func (s *Server) mediaDTO(m *store.Media) map[string]any {
	return map[string]any{
		"id": m.ID, "filename": m.Filename, "mime": m.MIME, "size": m.Size,
		"url":         strings.TrimRight(s.cfg.BaseURL, "/") + "/media/" + m.Path,
		"markdown":    "![" + m.Filename + "](/media/" + m.Path + ")",
		"uploaded_at": m.CreatedAt,
	}
}

// handleUploadMedia 接收 multipart 上传。返回体里直接给一段可粘贴的
// Markdown —— AI 客户端拿到就能插进正文，省掉一轮"URL 该怎么拼"的推理。
func (s *Server) handleUploadMedia(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxMediaSize+1<<20)
	if err := r.ParseMultipartForm(store.MaxMediaSize); err != nil {
		badRequest(w, "expected multipart/form-data with a 'file' field: %s", err)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		badRequest(w, "missing 'file' field")
		return
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, store.MaxMediaSize+1))
	if err != nil {
		fail(w, err)
		return
	}
	mime := hdr.Header.Get("Content-Type")
	if v := r.FormValue("mime"); v != "" {
		mime = v
	}
	m, err := s.db.SaveMedia(r.Context(), s.cfg.MediaRoot, userFrom(r.Context()).ID,
		hdr.Filename, strings.ToLower(strings.TrimSpace(mime)), data)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.mediaDTO(m))
}
