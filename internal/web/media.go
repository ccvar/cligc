package web

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"cligc.com/internal/store"
)

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

	var ok, failed int
	var firstErr string
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			failed++
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, store.MaxMediaSize+1))
		f.Close()
		if err != nil {
			failed++
			continue
		}
		// 类型按字节内容判断，不信浏览器声明的 Content-Type，
		// 更不信扩展名——两者都是客户端可以随便写的。
		mime := http.DetectContentType(data)
		if i := strings.IndexByte(mime, ';'); i >= 0 {
			mime = mime[:i]
		}
		if _, err := s.db.SaveMedia(r.Context(), s.cfg.MediaRoot,
			userFrom(r.Context()).ID, fh.Filename, mime, data); err != nil {
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		ok++
	}

	msg := s.tr(r, "admin.media.uploaded", strconv.Itoa(ok))
	if failed > 0 {
		msg += s.tr(r, "admin.media.uploadFailed", strconv.Itoa(failed))
		if firstErr != "" {
			msg += "：" + firstErr
		}
	}
	redirectFlash(w, r, "/admin/media", msg)
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
