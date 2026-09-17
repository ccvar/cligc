package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 评论状态。
const (
	CommentPending  = "pending"
	CommentApproved = "approved"
	CommentSpam     = "spam"
)

// 评论长度限制。上限不是为了省空间，是为了让灌水的成本高于收益。
const (
	MaxCommentLen = 2000
	MinCommentLen = 2
)

// Comment 是一条评论。
//
// Body 永远是纯文本。展示时由模板整体转义，不解析 Markdown、不自动识别 URL。
type Comment struct {
	ID         int64
	PostID     int64
	UserID     *int64 // nil = 匿名访客
	AuthorName string
	Body       string
	Status     string
	CreatedAt  time.Time

	// 审核队列里需要知道评论挂在哪篇文章下
	PostTitle string
	PostSlug  string
}

// IsAnonymous 报告这条评论是否来自未登录访客。
func (c *Comment) IsAnonymous() bool { return c.UserID == nil }

// CreateCommentInput 是发表评论的入参。
type CreateCommentInput struct {
	PostID     int64
	UserID     *int64 // 登录用户传其 id，匿名传 nil
	AuthorName string
	Body       string
}

// CreateComment 发表一条评论。
//
// 登录用户的评论直接 approved——站上的用户都是自己开的账号，不是开放注册。
// 匿名评论一律 pending：开放的评论区不做人工审核，三个月就会变成垃圾场，
// 而搜索引擎把"疏于管理的 UGC 垃圾"明确列为站点级的降权理由。
func (d *DB) CreateComment(ctx context.Context, in CreateCommentInput) (*Comment, error) {
	body := strings.TrimSpace(in.Body)
	if n := len([]rune(body)); n < MinCommentLen {
		return nil, fmt.Errorf("%w: comment is too short", ErrInvalidInput)
	} else if n > MaxCommentLen {
		return nil, fmt.Errorf("%w: comment exceeds %d characters", ErrInvalidInput, MaxCommentLen)
	}
	name := strings.TrimSpace(in.AuthorName)
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}

	status := CommentPending
	if in.UserID != nil {
		status = CommentApproved
		if name == "" {
			if u, err := d.UserByID(ctx, *in.UserID); err == nil {
				name = u.Name
			}
		}
	}
	if name == "" {
		name = "匿名"
	}

	// 只允许给已发布的文章留言。草稿和归档都不该有公开评论入口，
	// 拦在这里是因为前端表单可以被绕过。
	var status0 string
	if err := d.R.QueryRowContext(ctx, `select status from posts where id=?`, in.PostID).Scan(&status0); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if status0 != StatusPublished {
		return nil, fmt.Errorf("%w: comments are only open on published posts", ErrForbidden)
	}

	n := now()
	res, err := d.W.ExecContext(ctx,
		`insert into comments(post_id,user_id,author_name,body,status,created_at) values(?,?,?,?,?,?)`,
		in.PostID, in.UserID, name, body, status, n)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Comment{ID: id, PostID: in.PostID, UserID: in.UserID, AuthorName: name,
		Body: body, Status: status, CreatedAt: ts(n)}, nil
}

const commentCols = `c.id,c.post_id,c.user_id,c.author_name,c.body,c.status,c.created_at`

func scanComment(sc interface{ Scan(...any) error }, withPost bool) (*Comment, error) {
	var c Comment
	var uid sql.NullInt64
	var created int64
	var err error
	if withPost {
		err = sc.Scan(&c.ID, &c.PostID, &uid, &c.AuthorName, &c.Body, &c.Status, &created,
			&c.PostTitle, &c.PostSlug)
	} else {
		err = sc.Scan(&c.ID, &c.PostID, &uid, &c.AuthorName, &c.Body, &c.Status, &created)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if uid.Valid {
		v := uid.Int64
		c.UserID = &v
	}
	c.CreatedAt = ts(created)
	return &c, nil
}

// CommentsForPost 返回某篇文章下已通过审核的评论，按时间正序。
func (d *DB) CommentsForPost(ctx context.Context, postID int64) ([]Comment, error) {
	rows, err := d.R.QueryContext(ctx,
		`select `+commentCols+` from comments c
		  where c.post_id=? and c.status=? order by c.created_at`, postID, CommentApproved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		c, err := scanComment(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// CommentFilter 限定审核队列的查询范围。
type CommentFilter struct {
	Status string // 空表示全部状态
	// AuthorID 非 0 时只返回挂在该作者文章下的评论。后台里作者只该看到
	// 自己文章下的评论，管理员才看全站。
	AuthorID int64
	Limit    int
	Offset   int
}

// ListComments 分页列出评论，带上所属文章，用于审核队列。
func (d *DB) ListComments(ctx context.Context, f CommentFilter) ([]Comment, int, error) {
	conds, args := []string{"1=1"}, []any{}
	if f.Status != "" {
		conds = append(conds, "c.status=?")
		args = append(args, f.Status)
	}
	if f.AuthorID != 0 {
		conds = append(conds, "p.user_id=?")
		args = append(args, f.AuthorID)
	}
	where := strings.Join(conds, " and ")
	limit, offset := f.Limit, f.Offset
	var total int
	if err := d.R.QueryRowContext(ctx,
		`select count(*) from comments c join posts p on p.id=c.post_id
		  where `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := d.R.QueryContext(ctx,
		`select `+commentCols+`, p.title, p.slug from comments c
		   join posts p on p.id=c.post_id
		  where `+where+` order by c.created_at desc limit ? offset ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		c, err := scanComment(rows, true)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *c)
	}
	return out, total, rows.Err()
}

// CountComments 返回某状态的评论数，用于在后台显示待审数量。
func (d *DB) CountComments(ctx context.Context, status string) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `select count(*) from comments where status=?`, status).Scan(&n)
	return n, err
}

// SetCommentStatus 审核一条评论。只有管理员和文章作者能操作。
func (d *DB) SetCommentStatus(ctx context.Context, a Actor, id int64, status string) error {
	switch status {
	case CommentApproved, CommentPending, CommentSpam:
	default:
		return fmt.Errorf("%w: unknown comment status %q", ErrInvalidInput, status)
	}
	return d.tx(ctx, func(t *sql.Tx) error {
		var owner int64
		err := t.QueryRowContext(ctx,
			`select p.user_id from comments c join posts p on p.id=c.post_id where c.id=?`, id).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !a.canTouch(owner) {
			return ErrForbidden
		}
		_, err = t.ExecContext(ctx, `update comments set status=? where id=?`, status, id)
		return err
	})
}

// DeleteComment 删除一条评论。
func (d *DB) DeleteComment(ctx context.Context, a Actor, id int64) error {
	return d.tx(ctx, func(t *sql.Tx) error {
		var owner int64
		err := t.QueryRowContext(ctx,
			`select p.user_id from comments c join posts p on p.id=c.post_id where c.id=?`, id).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !a.canTouch(owner) {
			return ErrForbidden
		}
		_, err = t.ExecContext(ctx, `delete from comments where id=?`, id)
		return err
	})
}

// PendingDuplicate 报告同一篇文章下是否已经有一条一模一样的待审评论。
//
// 限速按来源聚合，所以换一批地址就能绕过去；而刷评论的脚本几乎总是重复
// 提交同一段文字。按内容去重挡的正是这一类——它和限速是互补的两个维度，
// 一个看"谁发的"，一个看"发的是什么"。
//
// 只看待审的：已通过的重复内容说明人工放行过，不该再被当成垃圾。
func (d *DB) PendingDuplicate(ctx context.Context, postID int64, body string) (bool, error) {
	var n int
	err := d.R.QueryRowContext(ctx,
		`select count(*) from comments where post_id=? and status=? and body=? limit 1`,
		postID, CommentPending, strings.TrimSpace(body)).Scan(&n)
	return n > 0, err
}
