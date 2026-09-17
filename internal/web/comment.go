package web

import (
	"net/http"
	"strconv"
	"strings"

	"cligc.com/internal/store"
)

// 匿名评论的限速窗口。登录用户不受限——站上的账号都是自己开的。
const (
	commentMaxPerWindow = 3
)

// handleCommentSubmit 接收文章页的评论提交。
//
// 匿名评论一律进 pending 队列。开放的评论区不做人工审核，三个月就会变成
// 垃圾场，而搜索引擎把"疏于管理的 UGC 垃圾"明确列为站点级降权理由——
// 这不是洁癖，是整站的排名风险。
func (s *Server) handleCommentSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.commentsOn(r) {
		s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.commentsOff"))
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	slug := r.PathValue("slug")
	p, err := s.db.PostBySlug(r.Context(), slug)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, s.tr(r, "err.postNotFound"))
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}

	// 蜜罐字段：页面上对人不可见，机器人会照填不误。命中就当作成功处理
	// 但什么也不写——让机器人以为得逞了，比返回错误更能减少重试。
	if strings.TrimSpace(r.FormValue("website")) != "" {
		http.Redirect(w, r, "/p/"+slug+"#comments", http.StatusSeeOther)
		return
	}

	u := userFrom(r.Context())
	var uid *int64
	if u != nil {
		uid = &u.ID
	} else {
		// 三道闸门，挡的是三个不同的维度：
		//   限速   → 单个来源发得多快（按 /64 聚合，见 rateKey）
		//   队列   → 全站积压总量，挡"一万个来源各发三条"
		//   去重   → 内容本身，挡换了地址但照抄同一段文字的脚本
		// 少任何一道，另外两道都能被绕过去。
		if !s.login.allow("comment:"+rateKey(clientIP(r, s.cfg.TrustProxy)), commentMaxPerWindow) {
			s.renderCommentError(w, r, p, s.tr(r, "comment.tooFast"))
			return
		}
		if cap := s.cfg.CommentQueueCap; cap > 0 {
			if n, err := s.db.CountComments(r.Context(), store.CommentPending); err == nil && n >= cap {
				s.renderCommentError(w, r, p, s.tr(r, "comment.queueFull"))
				return
			}
		}
		// 重复内容按蜜罐的方式处理：返回成功但什么也不写。
		// 明确报错等于告诉脚本"换段文字再来"，静默丢弃不给这个反馈。
		if dup, err := s.db.PendingDuplicate(r.Context(), p.ID, r.FormValue("body")); err == nil && dup {
			redirectFlash(w, r, "/p/"+slug, s.tr(r, "comment.queued"))
			return
		}
	}

	if _, err := s.db.CreateComment(r.Context(), store.CreateCommentInput{
		PostID: p.ID, UserID: uid,
		AuthorName: r.FormValue("author_name"),
		Body:       r.FormValue("body"),
	}); err != nil {
		s.renderCommentError(w, r, p, s.tr(r, "comment.failed", err.Error()))
		return
	}

	msg := s.tr(r, "comment.queued")
	if uid != nil {
		msg = s.tr(r, "comment.posted")
	}
	redirectFlash(w, r, "/p/"+slug, msg)
}

// renderCommentError 把错误连同文章一起渲染回去，保留用户已经输入的内容。
func (s *Server) renderCommentError(w http.ResponseWriter, r *http.Request, p *store.Post, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	comments, _ := s.db.CommentsForPost(r.Context(), p.ID)
	s.render(w, r, "post.html", page{
		Title: p.Title, Desc: p.Summary, NoIndex: true,
		Canonical: s.cfg.BaseURL + "/p/" + p.Slug,
		Flash:     msg,
		Data: map[string]any{
			"Post": p, "Comments": comments,
			"CommentsEnabled": s.commentsOn(r),
			"DraftBody":       r.FormValue("body"),
			"DraftName":       r.FormValue("author_name"),
		},
	})
}

// --- 后台审核 ---

// handleCommentQueue 是评论审核队列。
func (s *Server) handleCommentQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	status := r.URL.Query().Get("status")
	if status == "" {
		status = store.CommentPending // 默认直接展示待审的，那才是要干活的地方
	}
	if status == "all" {
		status = ""
	}
	pg := pageParam(r)

	// 作者只看自己文章下的评论，管理员看全站
	var scope int64
	if u := userFrom(ctx); u != nil && !u.IsAdmin() {
		scope = u.ID
	}
	items, total, err := s.db.ListComments(ctx, store.CommentFilter{
		Status: status, AuthorID: scope,
		Limit: s.cfg.PerPage, Offset: (pg - 1) * s.cfg.PerPage,
	})
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	pending, _ := s.db.CountComments(ctx, store.CommentPending)
	prev, next := pager("/admin/comments", nil, pg, s.cfg.PerPage, total)

	s.render(w, r, "admin_comments.html", page{
		Title: s.tr(r, "admin.comments.title"), NoIndex: true, Wide: true, AdminTab: "comments",
		Flash: r.URL.Query().Get("flash"),
		Prev:  prev, Next: next,
		Data: map[string]any{
			"Comments": items, "Total": total, "Pending": pending,
			"Status": r.URL.Query().Get("status"),
		},
	})
}

// handleCommentModerate 通过 / 标记垃圾 / 删除一条评论。
func (s *Server) handleCommentModerate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badID"))
		return
	}
	action := r.PathValue("action")
	back := "/admin/comments"
	if q := r.URL.Query().Get("status"); q != "" {
		back += "?status=" + q
	}

	switch action {
	case "approve":
		err = s.db.SetCommentStatus(r.Context(), actorOf(r), id, store.CommentApproved)
	case "spam":
		err = s.db.SetCommentStatus(r.Context(), actorOf(r), id, store.CommentSpam)
	case "pending":
		err = s.db.SetCommentStatus(r.Context(), actorOf(r), id, store.CommentPending)
	case "delete":
		err = s.db.DeleteComment(r.Context(), actorOf(r), id)
	default:
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badID"))
		return
	}
	if err != nil {
		redirectFlash(w, r, back, s.tr(r, "flash.opFailed", err.Error()))
		return
	}
	redirectFlash(w, r, back, map[string]string{
		"approve": s.tr(r, "admin.comments.statusApproved"),
		"spam":    s.tr(r, "admin.comments.statusSpam"),
		"pending": s.tr(r, "admin.comments.statusPending"),
		"delete":  s.tr(r, "flash.deleted"),
	}[action])
}
