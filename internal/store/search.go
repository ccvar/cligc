package store

import (
	"context"
	"strings"

	"cligc.com/internal/render"
	"cligc.com/internal/tokenize"
)

// SearchFilter 限定检索范围。
//
// 零值表示"全站、全状态"，调用方几乎总该收窄：公开检索传
// Status: StatusPublished，后台"搜我的草稿"传 UserID + 空 Status。
// 把范围做成显式参数而不是内置默认，是为了让漏写的地方在 code review
// 里看得见——默认只搜已发布很安全，但会让后台搜不到草稿，那是个难查的 bug。
type SearchFilter struct {
	UserID int64
	Status string
	Lang   string // 空表示不限语言
	Limit  int
	Offset int
}

// Search 在已发布文章里做全文检索，按 BM25 相关度排序。
//
// 查询串经 tokenize.Query 转成 FTS5 表达式：中文段落切成 bigram 短语，
// 要求在索引中按序相邻，效果接近子串匹配；不同段落之间是 AND。
// 用户输入里的引号已在 tokenize 层转义，不存在 FTS 语法注入。
//
// 注意 MATCH 左侧必须写 FTS 表的真实名字，SQLite 不接受别名。
func (d *DB) Search(ctx context.Context, q string, f SearchFilter) ([]SearchHit, int, error) {
	match := tokenize.Query(q)
	if match == "" {
		return nil, 0, nil
	}
	limit := f.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	cond := []string{"post_fts match ?"}
	args := []any{match}
	if f.Status != "" {
		cond = append(cond, "p.status=?")
		args = append(args, f.Status)
	}
	if f.UserID != 0 {
		cond = append(cond, "p.user_id=?")
		args = append(args, f.UserID)
	}
	where := strings.Join(cond, " and ")

	var total int
	if err := d.R.QueryRowContext(ctx,
		`select count(*) from post_fts join posts p on p.id=post_fts.rowid
		  where `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	// bm25 排序：FTS5 的 rank 越小越相关，升序即为最佳优先。
	rows, err := d.R.QueryContext(ctx,
		`select `+postColsList+`, p.body_text
		   from post_fts
		   join posts p on p.id=post_fts.rowid
		   join users u on u.id=p.user_id
		   left join categories c on c.id=p.category_id
		  where `+where+`
		  order by post_fts.rank
		  limit ? offset ?`, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	terms := tokenize.Terms(q)
	var out []SearchHit
	for rows.Next() {
		// body_text 是查询里追加在 postColsList 之后的额外一列，
		// 交给 scanPostWith 一起读——列的顺序只有它一个地方知道。
		var bodyText string
		p, err := scanPostWith(rows, &bodyText)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, SearchHit{
			Post:      *p,
			TitleHTML: render.Highlight(p.Title, terms, 200),
			Snippet:   render.Highlight(bodyText, terms, 120),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	// 检索结果也要带标签：它们和列表页一样承担站内导航
	posts := make([]Post, len(out))
	for i := range out {
		posts[i] = out[i].Post
	}
	if err := d.attachTags(ctx, posts); err != nil {
		return nil, 0, err
	}
	for i := range out {
		out[i].Post.Tags = posts[i].Tags
	}
	return out, total, nil
}
