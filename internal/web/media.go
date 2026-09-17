package web

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"cligc.com/internal/store"
)

// writeJSON 写一条 JSON 响应。后台里只有选图弹窗这一条路走 JSON，
// 不值得为它引一套框架。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleMediaList 是媒体库：已上传文件的网格 + 上传表单。
func (s *Server) handleMediaList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	pg := pageParam(r)
	const perPage = 24

	// 管理员看全站，作者只看自己传的
	var scope int64
	if !u.IsAdmin() {
		scope = u.ID
	}
	total, err := s.db.CountMedia(ctx, scope)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	items, err := s.db.ListMediaPage(ctx, scope, perPage, (pg-1)*perPage)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	prev, next := pager("/admin/media", nil, pg, perPage, total)

	s.render(w, r, "admin_media.html", page{
		Title: s.tr(r, "admin.media.title"), NoIndex: true, Wide: true, AdminTab: "media",
		Flash: r.URL.Query().Get("flash"),
		Prev:  prev, Next: next,
		Data: map[string]any{
			"Media": items, "Total": total,
			"MaxMB": store.MaxMediaSize / (1 << 20),
		},
	})
}

// handleMediaUpload 接收后台的文件上传。
func (s *Server) handleMediaUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxMediaSize+(1<<20))
	if err := r.ParseMultipartForm(store.MaxMediaSize); err != nil {
		redirectFlash(w, r, "/admin/media", s.tr(r, "admin.media.tooBig"))
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		redirectFlash(w, r, "/admin/media", s.tr(r, "admin.media.noFile"))
		return
	}

	saved, firstErr := s.saveUploads(r, r.MultipartForm.File["file"])
	msg := s.tr(r, "admin.media.uploaded", strconv.Itoa(len(saved)))
	if n := len(files) - len(saved); n > 0 {
		msg += s.tr(r, "admin.media.uploadFailed", strconv.Itoa(n))
		if firstErr != "" {
			msg += "：" + firstErr
		}
	}
	redirectFlash(w, r, "/admin/media", msg)
}

// saveUploads 把一批上传的文件存进媒体库，返回存成功的那些和第一条错误。
//
// 一批里坏了一个不该让其余的也白传——上传十张图因为其中一张是 HEIC 而
// 整批失败，是最让人恼火的那种失败。
func (s *Server) saveUploads(r *http.Request, files []*multipart.FileHeader) ([]*store.Media, string) {
	var out []*store.Media
	var firstErr string
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, store.MaxMediaSize+1))
		f.Close()
		if err != nil {
			continue
		}
		// 类型按字节内容判断，不信浏览器声明的 Content-Type，
		// 更不信扩展名——两者都是客户端可以随便写的。
		mime := http.DetectContentType(data)
		if i := strings.IndexByte(mime, ';'); i >= 0 {
			mime = mime[:i]
		}
		m, err := s.db.SaveMedia(r.Context(), s.cfg.MediaRoot,
			userFrom(r.Context()).ID, fh.Filename, mime, data, s.cfg.ImageMaxDim)
		if err != nil {
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		out = append(out, m)
	}
	return out, firstErr
}

// mediaJSON 是给编辑器里的选图弹窗用的一条记录。
//
// 和 API 的 mediaDTO 分开：那个是对外契约，改动要考虑别人的脚本；
// 这个只服务于同一个仓库里的一段 JS，字段可以随界面一起改。
type mediaJSON struct {
	ID   int64  `json:"id"`
	URL  string `json:"url"`
	Name string `json:"name"`
	W    int    `json:"w"`
	H    int    `json:"h"`
}

func toMediaJSON(items []store.Media) []mediaJSON {
	out := make([]mediaJSON, 0, len(items))
	for _, m := range items {
		out = append(out, mediaJSON{ID: m.ID, URL: "/media/" + m.Path,
			Name: m.Filename, W: m.Width, H: m.Height})
	}
	return out
}

// handleMediaPick 给编辑器的选图弹窗提供图片列表。
func (s *Server) handleMediaPick(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	var scope int64
	if !u.IsAdmin() {
		scope = u.ID
	}
	items, err := s.db.ListMediaPage(r.Context(), scope, 60, 0)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": toMediaJSON(items)})
}

// handleMediaPickUpload 是弹窗和拖拽/粘贴共用的上传口，返回 JSON。
//
// 和表单那条路分开是因为返回形态不同：表单要跳转加一条 flash，
// 这里要的是刚存下来的那张图的地址，好当场插进正文。
func (s *Server) handleMediaPickUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxMediaSize+(1<<20))
	if err := r.ParseMultipartForm(store.MaxMediaSize); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge,
			map[string]any{"error": s.tr(r, "admin.media.tooBig")})
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": s.tr(r, "admin.media.noFile")})
		return
	}
	saved, firstErr := s.saveUploads(r, files)
	if len(saved) == 0 {
		if firstErr == "" {
			firstErr = s.tr(r, "err.internal")
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": firstErr})
		return
	}
	items := make([]store.Media, 0, len(saved))
	for _, m := range saved {
		items = append(items, *m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": toMediaJSON(items)})
}

// handleMediaDelete 删除一个上传文件。
func (s *Server) handleMediaDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badID"))
		return
	}
	if err := s.db.DeleteMedia(r.Context(), actorOf(r), s.cfg.MediaRoot, id); err != nil {
		redirectFlash(w, r, "/admin/media", s.tr(r, "flash.opFailed", err.Error()))
		return
	}
	redirectFlash(w, r, "/admin/media", s.tr(r, "admin.media.deleted"))
}
