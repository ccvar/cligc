package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cligc.com/internal/i18n"
	"cligc.com/internal/render"
	"cligc.com/internal/tokenize"
)

// Actor 描述一次写操作的发起者。Kind 会写进 publish_events 做审计，
// 让你事后能分清哪些文章是从 AI 客户端发出去的。
type Actor struct {
	UserID  int64
	IsAdmin bool
	Kind    string // web | api
}

func (a Actor) canTouch(ownerID int64) bool { return a.IsAdmin || a.UserID == ownerID }

// CreatePostInput 是新建文章的入参。留空的字段走默认值。
type CreatePostInput struct {
	Title        string
	BodyMD       string
	Slug         string // 留空则由标题推导
	Summary      string // 留空则自动截取
	Tags         []string
	Source       string // 留空按 human 处理
	CanonicalURL string
	Lang         string // 留空按默认语言
	Indexable    *bool  // nil 表示默认可索引
	// CategorySlug 留空表示未分类。用 slug 而不是 ID：API 和表单里
	// 写 "essays" 比写 "3" 可读得多，也不会因为换库就失效。
	CategorySlug string

	// CoverMediaID 是封面图在媒体库里的 ID，nil 表示没有封面。
	// CoverAlt 是替代文字：封面会进 og:image，也会出现在列表卡片上，
	// 读屏软件和图片加载失败时靠它。
	CoverMediaID *int64
	CoverAlt     string

	// IdempotencyKey 非空时，同一用户 24 小时内重复提交相同 key 会直接
	// 返回首次创建的那篇文章，而不是再建一篇。AI 客户端会重试，没有这个
	// 机制迟早在站上堆出一串一模一样的草稿。
	IdempotencyKey string
}

// UpdatePostInput 是部分更新的入参：nil 表示"不改这一项"。
type UpdatePostInput struct {
	Title        *string
	BodyMD       *string
	Slug         *string
	Summary      *string
	Tags         *[]string
	Source       *string
	CanonicalURL *string
	Lang         *string
	Indexable    *bool
	// CategorySlug 指向空串表示取消分类，nil 表示不改。
	CategorySlug *string
	// CoverMediaID 指向 0 表示取掉封面，nil 表示不改。
	// 用 0 而不是另加一个 ClearCover 布尔：媒体 ID 从 1 开始，0 不是
	// 任何一张图，语义没有歧义。
	CoverMediaID *int64
	CoverAlt     *string
}

// CreatePost 新建一篇文章。永远创建为草稿——发布是独立的一步。
func (d *DB) CreatePost(ctx context.Context, a Actor, in CreatePostInput) (*Post, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, fmt.Errorf("%w: title required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.BodyMD) == "" {
		return nil, fmt.Errorf("%w: body required", ErrInvalidInput)
	}
	src := normalizeSource(in.Source)
	lang := normalizeLang(in.Lang)
	idx := true
	if in.Indexable != nil {
		idx = *in.Indexable
	}

	var id int64
	err := d.tx(ctx, func(t *sql.Tx) error {
		// 幂等：命中已有 key 直接复用之前那篇
		if in.IdempotencyKey != "" {
			var prev int64
			err := t.QueryRowContext(ctx,
				`select post_id from idempotency where user_id=? and key=?`,
				a.UserID, in.IdempotencyKey).Scan(&prev)
			if err == nil {
				id = prev
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		base := in.Slug
		if base == "" {
			base = Slugify(in.Title)
		} else {
			base = Slugify(base)
		}
		slug, err := uniqueSlug(ctx, t, "posts", base, 0)
		if err != nil {
			return err
		}

		r := d.rend.Render(in.BodyMD)
		summary := in.Summary
		if summary == "" {
			summary = r.Summary
		}
		n := now()
		// 分类给的是 slug，这里换成 ID。找不到就当未分类——AI 客户端写错
		// 一个分类名不该让整次创建失败，草稿存下来、人在后台改一下就行。
		catID, err := categoryIDBySlug(ctx, t, in.CategorySlug)
		if err != nil {
			return err
		}
		res, err := t.ExecContext(ctx,
			`insert into posts(user_id,slug,title,summary,body_md,body_html,body_text,toc_json,
			                   status,source,indexable,canonical_url,lang,category_id,
			                   cover_media_id,cover_alt,word_count,created_at,updated_at)
			 values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			a.UserID, slug, in.Title, summary, in.BodyMD, r.HTML, r.Plain, encodeTOC(r.Headings),
			StatusDraft, src, boolInt(idx), in.CanonicalURL, lang, catID,
			coverRef(in.CoverMediaID), strings.TrimSpace(in.CoverAlt), r.Words, n, n)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()

		if err := syncFTS(ctx, t, id, in.Title, r.Plain); err != nil {
			return err
		}
		if err := setTags(ctx, t, id, in.Tags); err != nil {
			return err
		}
		if in.IdempotencyKey != "" {
			if _, err := t.ExecContext(ctx,
				`insert into idempotency(user_id,key,post_id,created_at) values(?,?,?,?)`,
				a.UserID, in.IdempotencyKey, id, n); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d.PostByID(ctx, id)
}

// UpdatePost 部分更新一篇文章。正文变更会重新渲染并同步全文索引。
func (d *DB) UpdatePost(ctx context.Context, a Actor, id int64, in UpdatePostInput) (*Post, error) {
	err := d.tx(ctx, func(t *sql.Tx) error {
		var ownerID int64
		var curMD, curTitle string
		err := t.QueryRowContext(ctx, `select user_id,title,body_md from posts where id=?`, id).
			Scan(&ownerID, &curTitle, &curMD)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !a.canTouch(ownerID) {
			return ErrForbidden
		}

		set := []string{"updated_at=?"}
		args := []any{now()}
		add := func(col string, v any) { set = append(set, col+"=?"); args = append(args, v) }

		title := curTitle
		if in.Title != nil {
			if strings.TrimSpace(*in.Title) == "" {
				return fmt.Errorf("%w: title cannot be empty", ErrInvalidInput)
			}
			title = *in.Title
			add("title", title)
		}
		// 空的 slug 表示"不改"，而不是"重新生成"。
		//
		// 改 URL 是破坏性操作：已有链接失效、已积累的收录作废。让它因为
		// 一个恰好为空的表单字段就发生，代价远大于少一个便利功能。
		// 要换 slug 就明确填一个新的。
		if in.Slug != nil && strings.TrimSpace(*in.Slug) != "" {
			s, err := uniqueSlug(ctx, t, "posts", Slugify(*in.Slug), id)
			if err != nil {
				return err
			}
			add("slug", s)
		}
		if in.Source != nil {
			add("source", normalizeSource(*in.Source))
		}
		if in.CanonicalURL != nil {
			add("canonical_url", *in.CanonicalURL)
		}
		if in.CategorySlug != nil {
			catID, err := categoryIDBySlug(ctx, t, *in.CategorySlug)
			if err != nil {
				return err
			}
			add("category_id", catID)
		}
		if in.Indexable != nil {
			add("indexable", boolInt(*in.Indexable))
		}
		if in.CoverMediaID != nil {
			add("cover_media_id", coverRef(in.CoverMediaID))
		}
		if in.CoverAlt != nil {
			add("cover_alt", strings.TrimSpace(*in.CoverAlt))
		}
		if in.Lang != nil {
			add("lang", normalizeLang(*in.Lang))
		}

		body := curMD
		bodyChanged := in.BodyMD != nil && *in.BodyMD != curMD
		if in.BodyMD != nil {
			if strings.TrimSpace(*in.BodyMD) == "" {
				return fmt.Errorf("%w: body cannot be empty", ErrInvalidInput)
			}
			body = *in.BodyMD
		}
		// 标题或正文任一变动都要重渲染（正文）与重建索引（两者都进 FTS）
		if bodyChanged || in.Title != nil || in.Summary != nil {
			r := d.rend.Render(body)
			if bodyChanged {
				add("body_md", body)
				add("body_html", r.HTML)
				add("body_text", r.Plain)
				add("toc_json", encodeTOC(r.Headings))
				add("word_count", r.Words)
			}
			switch {
			case in.Summary != nil && *in.Summary != "":
				add("summary", *in.Summary)
			case in.Summary != nil: // 显式传空串 = 重新自动生成
				add("summary", r.Summary)
			case bodyChanged:
				add("summary", r.Summary)
			}
			if err := syncFTS(ctx, t, id, title, r.Plain); err != nil {
				return err
			}
		}

		args = append(args, id)
		if _, err := t.ExecContext(ctx, `update posts set `+strings.Join(set, ",")+` where id=?`, args...); err != nil {
			return err
		}
		if in.Tags != nil {
			if err := setTags(ctx, t, id, *in.Tags); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p, err := d.PostByID(ctx, id)
	d.notifyChanged(p)
	return p, err
}

// PublishPost 把草稿转为已发布。
//
// dailyCap > 0 时，同一用户当天的发布次数达到上限就拒绝。这是刻意的架构级
// 约束：AI 技能包让批量生产变得极其容易，而搜索引擎对"规模化内容"的判定
// 不看你用没用 AI，只看量和质。把闸门做在服务端，比依赖自制力可靠。
//
// 只有 draft/archived -> published 的跃迁才计入配额并记一条事件；
// 重复发布一篇已发布的文章是空操作。
func (d *DB) PublishPost(ctx context.Context, a Actor, id int64, dailyCap int) (*Post, error) {
	err := d.tx(ctx, func(t *sql.Tx) error {
		var ownerID int64
		var status string
		err := t.QueryRowContext(ctx, `select user_id,status from posts where id=?`, id).Scan(&ownerID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !a.canTouch(ownerID) {
			return ErrForbidden
		}
		if status == StatusPublished {
			return nil // 幂等
		}

		if dailyCap > 0 {
			var n int
			if err := t.QueryRowContext(ctx,
				`select count(*) from publish_events where user_id=? and day=?`,
				ownerID, today()).Scan(&n); err != nil {
				return err
			}
			if n >= dailyCap {
				return fmt.Errorf("%w: %d/%d today", ErrDailyCap, n, dailyCap)
			}
		}

		n := now()
		// published_at 只在首次发布时设置，之后重新发布不改——
		// 否则 sitemap 和 RSS 的时间线会乱跳。
		if _, err := t.ExecContext(ctx,
			// 顺手清掉待发排期：文章一旦发出去，不管走的是哪条路（手动、
			// API、还是定时任务），那个排期就失效了。留着它的话，这篇日后
			// 一旦被撤回草稿，就会在下一轮扫描里自己重新发布一次。
			`update posts set status=?, published_at=coalesce(published_at,?), updated_at=?, publish_at=null where id=?`,
			StatusPublished, n, n, id); err != nil {
			return err
		}
		kind := a.Kind
		if kind == "" {
			kind = "web"
		}
		_, err = t.ExecContext(ctx,
			`insert into publish_events(post_id,user_id,actor,day,created_at) values(?,?,?,?,?)`,
			id, ownerID, kind, today(), n)
		return err
	})
	if err != nil {
		return nil, err
	}
	p, err := d.PostByID(ctx, id)
	d.notifyChanged(p)
	return p, err
}

// UnpublishPost 把文章退回草稿。
func (d *DB) UnpublishPost(ctx context.Context, a Actor, id int64) (*Post, error) {
	return d.SetStatus(ctx, a, id, StatusDraft)
}

// SetStatus 在草稿和归档之间切换状态。
//
// 刻意不接受 published：发布要过每日配额、要记审计事件，必须走 PublishPost。
// 如果这里能直接写 published，那道闸门就等于形同虚设——权限检查最怕的
// 就是留一条绕过它的后门函数。
//
// 归档的语义是"从所有列表和搜索索引里撤下，但保留 URL 可访问"。
// 直接删除会制造死链，那对已经被收录、被别人引用过的文章是更坏的结果。
func (d *DB) SetStatus(ctx context.Context, a Actor, id int64, status string) (*Post, error) {
	if status != StatusDraft && status != StatusArchived {
		return nil, fmt.Errorf("%w: status must be draft or archived (use PublishPost to publish)", ErrInvalidInput)
	}
	err := d.tx(ctx, func(t *sql.Tx) error {
		var ownerID int64
		if err := t.QueryRowContext(ctx, `select user_id from posts where id=?`, id).Scan(&ownerID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !a.canTouch(ownerID) {
			return ErrForbidden
		}
		_, err := t.ExecContext(ctx, `update posts set status=?, updated_at=? where id=?`, status, now(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	p, err := d.PostByID(ctx, id)
	d.notifyChanged(p)
	return p, err
}

// DeletePost 删除文章，同时清掉全文索引。
func (d *DB) DeletePost(ctx context.Context, a Actor, id int64) error {
	// 删之前先把文章读出来：删完就没有 slug 可以拿来通知了，而这恰恰是
	// 最该通知的一种变化——引擎需要来一趟才知道这个 URL 已经 404。
	gone, _ := d.PostByID(ctx, id)
	err := d.deletePost(ctx, a, id)
	if err == nil {
		d.notifyChanged(gone)
	}
	return err
}

func (d *DB) deletePost(ctx context.Context, a Actor, id int64) error {
	return d.tx(ctx, func(t *sql.Tx) error {
		var ownerID int64
		if err := t.QueryRowContext(ctx, `select user_id from posts where id=?`, id).Scan(&ownerID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !a.canTouch(ownerID) {
			return ErrForbidden
		}
		if _, err := t.ExecContext(ctx, `delete from post_fts where rowid=?`, id); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `delete from posts where id=?`, id)
		return err
	})
}

// PublishedToday 返回某用户今天已发布的篇数，用于在界面上显示剩余配额。
func (d *DB) PublishedToday(ctx context.Context, userID int64) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx,
		`select count(*) from publish_events where user_id=? and day=?`, userID, today()).Scan(&n)
	return n, err
}

// --- 读取 ---

// 两份列清单对应同一个 scanPost。列表查询不读正文：一页 20 篇文章的
// body_md + body_html 可能是几百 KB，读出来只为了丢掉，纯属浪费。
const postCols = `p.id,p.user_id,p.slug,p.title,p.summary,p.body_md,p.body_html,p.status,p.source,
	p.indexable,p.canonical_url,p.word_count,p.created_at,p.updated_at,p.published_at,p.featured_at,
	p.toc_json,p.lang,p.translation_key,p.publish_at,u.name,u.slug,
	p.category_id,coalesce(c.slug,''),coalesce(c.name,''),` + coverCols

const postColsList = `p.id,p.user_id,p.slug,p.title,p.summary,'' as body_md,'' as body_html,p.status,p.source,
	p.indexable,p.canonical_url,p.word_count,p.created_at,p.updated_at,p.published_at,p.featured_at,
	'' as toc_json,p.lang,p.translation_key,p.publish_at,u.name,u.slug,
	p.category_id,coalesce(c.slug,''),coalesce(c.name,''),` + coverCols

// coverCols 是封面那几列。列表页也要它——卡片上就是靠它出图。
const coverCols = `p.cover_media_id,p.cover_alt,
	coalesce(m.path,''),coalesce(m.width,0),coalesce(m.height,0)`

// postFrom 是取文章时固定的那串 join。写成一个常量而不是逐处重复：
// 加一张关联表要改七个地方时，漏掉一个的表现是某一个列表页少一列，
// 而那个页面未必有测试。
const postFrom = ` from posts p
	join users u on u.id=p.user_id
	left join categories c on c.id=p.category_id
	left join media m on m.id=p.cover_media_id`

func scanPost(sc interface{ Scan(...any) error }) (*Post, error) {
	return scanPostWith(sc)
}

// scanPostWith 按 postCols / postColsList 的顺序读出一篇文章，extra 是查询里
// 追加在这些列之后的额外目标。
//
// 有 extra 这个口子是因为检索查询要多带一列 body_text。它原本自己抄了一份
// 逐字段 Scan，结果给 posts 加列时漏改了那一份——列清单和 Scan 分散在两处，
// 迟早会对不上。现在只有这一个地方知道列的顺序。
func scanPostWith(sc interface{ Scan(...any) error }, extra ...any) (*Post, error) {
	var p Post
	var idx int
	var created, updated int64
	var pub, feat, sched, catID, coverID sql.NullInt64
	var toc string

	dest := []any{&p.ID, &p.UserID, &p.Slug, &p.Title, &p.Summary, &p.BodyMD, &p.BodyHTML,
		&p.Status, &p.Source, &idx, &p.CanonicalURL, &p.WordCount, &created, &updated, &pub, &feat,
		&toc, &p.Lang, &p.TransKey, &sched, &p.AuthorName, &p.AuthorSlug,
		&catID, &p.CategorySlug, &p.CategoryName,
		&coverID, &p.CoverAlt, &p.CoverPath, &p.CoverW, &p.CoverH}
	dest = append(dest, extra...)

	err := sc.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Indexable = idx != 0
	p.CreatedAt = ts(created)
	p.UpdatedAt = ts(updated)
	p.PublishedAt = nullTime(pub)
	p.FeaturedAt = nullTime(feat)
	p.PublishAt = nullTime(sched)
	if coverID.Valid {
		id := coverID.Int64
		p.CoverMediaID = &id
	}
	if catID.Valid {
		p.CategoryID = &catID.Int64
	}
	if toc != "" {
		// 解析失败就当没有大纲：目录是阅读辅助，不该因为它坏掉整个页面
		json.Unmarshal([]byte(toc), &p.Headings)
	}
	return &p, nil
}

// encodeTOC 把大纲序列化存库。空大纲存空串而不是 "null"，
// 读回来时少一次判断。
func encodeTOC(hs []render.Heading) string {
	if len(hs) == 0 {
		return ""
	}
	b, err := json.Marshal(hs)
	if err != nil {
		return ""
	}
	return string(b)
}

// PostByID 按 id 取文章（含标签）。
func (d *DB) PostByID(ctx context.Context, id int64) (*Post, error) {
	p, err := scanPost(d.R.QueryRowContext(ctx,
		`select `+postCols+postFrom+` where p.id=?`, id))
	if err != nil {
		return nil, err
	}
	return d.withTags(ctx, p)
}

// PostBySlug 按 slug 取文章（含标签）。
func (d *DB) PostBySlug(ctx context.Context, slug string) (*Post, error) {
	p, err := scanPost(d.R.QueryRowContext(ctx,
		`select `+postCols+postFrom+` where p.slug=?`, slug))
	if err != nil {
		return nil, err
	}
	return d.withTags(ctx, p)
}

func (d *DB) withTags(ctx context.Context, p *Post) (*Post, error) {
	rows, err := d.R.QueryContext(ctx,
		`select t.id,t.slug,t.name from tags t join post_tags pt on pt.tag_id=t.id
		 where pt.post_id=? order by t.name`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Slug, &t.Name); err != nil {
			return nil, err
		}
		p.Tags = append(p.Tags, t)
	}
	return p, rows.Err()
}

// ListFilter 描述一次列表查询。零值表示"不限制该项"。
type ListFilter struct {
	Status  string // draft | published | archived
	UserID  int64
	TagSlug string
	// CategorySlug 按分类筛选。"-" 表示只要未分类的那些——后台整理时
	// 最需要的就是"哪些还没归类"，而空串已经被"不限"占用了。
	CategorySlug string
	Lang         string // 空表示不限语言（后台用）；公开列表总是限定
	// ExcludeFeatured 把已精选的文章从结果里去掉。首页顶部单独有精选区，
	// 不排除的话同一篇会在一屏里出现两次。
	ExcludeFeatured bool
	Limit           int
	Offset          int
}

// ListPosts 返回一页文章和满足条件的总数（用于分页）。
// 列表不带正文，避免把整站正文读进内存。
func (d *DB) ListPosts(ctx context.Context, f ListFilter) ([]Post, int, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Status != "" {
		where = append(where, "p.status=?")
		args = append(args, f.Status)
	}
	if f.UserID != 0 {
		where = append(where, "p.user_id=?")
		args = append(args, f.UserID)
	}
	if f.Lang != "" {
		where = append(where, "p.lang=?")
		args = append(args, f.Lang)
	}
	if f.ExcludeFeatured {
		where = append(where, "p.featured_at is null")
	}
	join := ""
	if f.TagSlug != "" {
		join = ` join post_tags pt on pt.post_id=p.id join tags t on t.id=pt.tag_id `
		where = append(where, "t.slug=?")
		args = append(args, f.TagSlug)
	}
	// 分类不用 join：posts 上就是外键，子查询比多一次 join 便宜，
	// 而且不会和上面的标签 join 撞出笛卡尔积。
	switch f.CategorySlug {
	case "":
	case "-":
		where = append(where, "p.category_id is null")
	default:
		where = append(where, "p.category_id = (select id from categories where slug=?)")
		args = append(args, f.CategorySlug)
	}
	cond := strings.Join(where, " and ")

	var total int
	if err := d.R.QueryRowContext(ctx,
		`select count(*) from posts p`+join+` where `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	// 已发布的按发布时间排；草稿列表按最近修改排，这才是编辑时想要的顺序。
	order := "p.updated_at desc"
	if f.Status == StatusPublished {
		order = "p.published_at desc, p.id desc"
	}

	rows, err := d.R.QueryContext(ctx,
		`select `+postColsList+postFrom+join+
			` where `+cond+` order by `+order+` limit ? offset ?`,
		append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := d.attachTags(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// attachTags 用一次查询给一整页文章补上标签。
//
// 列表页的标签不只是装饰：它们是指向标签页的内链，而标签页又内链回
// 其它文章。这层内链是爬虫在站内游走的主要路径，逐篇再查一次
// （N+1）会让列表页慢到没法用，所以这里一次 IN 查询解决。
func (d *DB) attachTags(ctx context.Context, posts []Post) error {
	if len(posts) == 0 {
		return nil
	}
	idx := make(map[int64]*Post, len(posts))
	args := make([]any, 0, len(posts))
	ph := make([]string, 0, len(posts))
	for i := range posts {
		idx[posts[i].ID] = &posts[i]
		args = append(args, posts[i].ID)
		ph = append(ph, "?")
	}
	rows, err := d.R.QueryContext(ctx,
		`select pt.post_id, t.id, t.slug, t.name
		   from post_tags pt join tags t on t.id=pt.tag_id
		  where pt.post_id in (`+strings.Join(ph, ",")+`)
		  order by pt.post_id, t.name`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		var t Tag
		if err := rows.Scan(&pid, &t.ID, &t.Slug, &t.Name); err != nil {
			return err
		}
		if p := idx[pid]; p != nil {
			p.Tags = append(p.Tags, t)
		}
	}
	return rows.Err()
}

// ListTags 列出所有标签及其已发布文章数，按文章数降序。
func (d *DB) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := d.R.QueryContext(ctx,
		`select t.id,t.slug,t.name,count(p.id) as n
		   from tags t
		   join post_tags pt on pt.tag_id=t.id
		   join posts p on p.id=pt.post_id and p.status=?
		  group by t.id order by n desc, t.name`, StatusPublished)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Slug, &t.Name, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- 内部工具 ---

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// coverRef 把封面 ID 变成可以直接写进库的值：nil 和 0 都是"没有封面"。
//
// 必须回 nil 而不是 0：cover_media_id 是外键，写 0 会撞上"media 里没有
// id=0 这一行"而整条插入失败。
func coverRef(id *int64) any {
	if id == nil || *id <= 0 {
		return nil
	}
	return *id
}

// normalizeLang 把语言代码收敛到注册表里的合法标签，未知值退回默认语言。
//
// 校验的是**内容语言**注册表而不是已加载的界面语言：一篇斯瓦希里语文章
// 完全合法，哪怕界面还没翻译成斯瓦希里语。把这两者绑在一起，就等于规定
// "界面没翻译过的语言不许发文"，那显然是错的。
//
// 不报错是刻意的：语言是展示维度，一个拼错的值不该让整篇文章保存失败。
func normalizeLang(s string) string {
	if i18n.Known(s) {
		return s
	}
	return i18n.Default().Code
}

func normalizeSource(s string) string {
	switch s {
	case SourceAIAssisted, SourceAIGenerated, SourceHuman:
		return s
	default:
		return SourceHuman
	}
}

// syncFTS 重建一篇文章的全文索引行。存进去的是 bigram token 串而非原文。
func syncFTS(ctx context.Context, t *sql.Tx, id int64, title, plain string) error {
	if _, err := t.ExecContext(ctx, `delete from post_fts where rowid=?`, id); err != nil {
		return err
	}
	_, err := t.ExecContext(ctx,
		`insert into post_fts(rowid,title,body) values(?,?,?)`,
		id, tokenize.Index(title), tokenize.Index(plain))
	return err
}

// setTags 全量替换一篇文章的标签。
func setTags(ctx context.Context, t *sql.Tx, postID int64, names []string) error {
	if _, err := t.ExecContext(ctx, `delete from post_tags where post_id=?`, postID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" || len([]rune(name)) > 32 {
			continue
		}
		slug := TagSlug(name)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true

		var tagID int64
		err := t.QueryRowContext(ctx, `select id from tags where slug=?`, slug).Scan(&tagID)
		if errors.Is(err, sql.ErrNoRows) {
			res, err := t.ExecContext(ctx, `insert into tags(slug,name) values(?,?)`, slug, name)
			if err != nil {
				return err
			}
			tagID, _ = res.LastInsertId()
		} else if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx,
			`insert or ignore into post_tags(post_id,tag_id) values(?,?)`, postID, tagID); err != nil {
			return err
		}
	}
	return nil
}

// SitemapEntry 是 sitemap 里的一行。只取 slug 和时间，不读正文——
// sitemap 可能要遍历全站几万篇文章，读正文会把内存打爆。
type SitemapEntry struct {
	Slug      string
	Lang      string
	UpdatedAt time.Time
}

// CountIndexable 返回应当进入 sitemap 的文章数：已发布、允许索引、
// 且没有指向站外的 canonical。
func (d *DB) CountIndexable(ctx context.Context) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx,
		`select count(*) from posts where status=? and indexable=1 and canonical_url=''`,
		StatusPublished).Scan(&n)
	return n, err
}

// IndexableEntries 按发布时间倒序返回一页 sitemap 条目。
func (d *DB) IndexableEntries(ctx context.Context, limit, offset int) ([]SitemapEntry, error) {
	rows, err := d.R.QueryContext(ctx,
		`select slug, lang, updated_at from posts
		  where status=? and indexable=1 and canonical_url=''
		  order by published_at desc, id desc limit ? offset ?`,
		StatusPublished, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SitemapEntry
	for rows.Next() {
		var e SitemapEntry
		var u int64
		if err := rows.Scan(&e.Slug, &e.Lang, &u); err != nil {
			return nil, err
		}
		e.UpdatedAt = ts(u)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RerenderAll 重新渲染所有文章的 HTML 缓存并重建全文索引，返回处理篇数。
//
// body_html 和 post_fts 都是 body_md 的派生物。渲染器一旦变化（换了扩展、
// 改了外链规则、加了代码高亮），旧文章的缓存就过时了——而这种过时不会
// 报错，只会表现为"新文章有高亮、老文章没有"。有了这条路径，渲染器的
// 任何改动都有对应的补救手段。
//
// 刻意不碰 summary：它可能是作者手写的，无法区分"自动生成"和"手工覆盖"，
// 重算会默默毁掉人工写的摘要。
func (d *DB) RerenderAll(ctx context.Context) (int, error) {
	rows, err := d.R.QueryContext(ctx, `select id, title, body_md from posts order by id`)
	if err != nil {
		return 0, err
	}
	type item struct {
		id          int64
		title, body string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.title, &it.body); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, it := range items {
		r := d.rend.Render(it.body)
		err := d.tx(ctx, func(t *sql.Tx) error {
			if _, err := t.ExecContext(ctx,
				`update posts set body_html=?, body_text=?, toc_json=?, word_count=? where id=?`,
				r.HTML, r.Plain, encodeTOC(r.Headings), r.Words, it.id); err != nil {
				return err
			}
			return syncFTS(ctx, t, it.id, it.title, r.Plain)
		})
		if err != nil {
			return n, fmt.Errorf("post %d: %w", it.id, err)
		}
		n++
	}
	return n, nil
}

// MaxFeatured 是首页精选区实际展示的条数。
//
// 超出的部分仍然保留精选标记但不展示——与其在置顶时报错让人猜"为什么不让我
// 加"，不如让界面如实显示"已精选 N 篇，首页展示前 3 篇"。
const MaxFeatured = 3

// SetFeatured 把文章置顶到首页精选区，或取消置顶。
//
// 只有已发布的文章能被精选：精选区是首页最显眼的位置，草稿出现在那里
// 等于绕过了发布这道闸门。
func (d *DB) SetFeatured(ctx context.Context, a Actor, id int64, on bool) (*Post, error) {
	err := d.tx(ctx, func(t *sql.Tx) error {
		var ownerID int64
		var status string
		err := t.QueryRowContext(ctx, `select user_id,status from posts where id=?`, id).Scan(&ownerID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !a.canTouch(ownerID) {
			return ErrForbidden
		}
		if on && status != StatusPublished {
			return fmt.Errorf("%w: only published posts can be featured", ErrForbidden)
		}
		var v *int64
		if on {
			n := now()
			v = &n
		}
		_, err = t.ExecContext(ctx, `update posts set featured_at=? where id=?`, v, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return d.PostByID(ctx, id)
}

// FeaturedPosts 返回某语言的精选文章，最近置顶的排在前面。
//
// 按语言过滤：英文首页出现三篇中文精选，对读者是噪音、对搜索引擎是
// 语言信号混乱。
func (d *DB) FeaturedPosts(ctx context.Context, lang string, limit int) ([]Post, error) {
	if limit <= 0 {
		limit = MaxFeatured
	}
	rows, err := d.R.QueryContext(ctx,
		`select `+postColsList+postFrom+`
		  where p.featured_at is not null and p.status=? and p.lang=?
		  order by p.featured_at desc limit ?`, StatusPublished, lang, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, d.attachTags(ctx, out)
}

// CountFeatured 返回已精选的篇数，用于在后台如实显示"超出的不展示"。
func (d *DB) CountFeatured(ctx context.Context) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx,
		`select count(*) from posts where featured_at is not null and status=?`,
		StatusPublished).Scan(&n)
	return n, err
}

// LinkTranslation 把 id 关联为 ofID 的另一语言版本；ofID 为 0 表示解除关联。
//
// 分组是对称的：两篇共享一个 translation_key，没有谁是"原文"。如果目标那篇
// 还没有 key 就当场生成一个，两篇一起写进去。
//
// 刻意不做"同一语言只能有一篇"的约束：同一内容确实可能有两个中文版本
// （比如一份精简版），强行禁止只会逼人绕开这个字段。真出现重复时
// hreflang 会指到其中一篇，那是内容决策不是数据完整性问题。
func (d *DB) LinkTranslation(ctx context.Context, a Actor, id, ofID int64) error {
	return d.tx(ctx, func(t *sql.Tx) error {
		var ownerID int64
		if err := t.QueryRowContext(ctx, `select user_id from posts where id=?`, id).Scan(&ownerID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !a.canTouch(ownerID) {
			return ErrForbidden
		}
		if ofID == 0 {
			_, err := t.ExecContext(ctx, `update posts set translation_key='' where id=?`, id)
			return err
		}
		if ofID == id {
			return fmt.Errorf("%w: a post cannot be a translation of itself", ErrInvalidInput)
		}

		var key string
		if err := t.QueryRowContext(ctx,
			`select translation_key from posts where id=?`, ofID).Scan(&key); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no post with id %d", ErrNotFound, ofID)
			}
			return err
		}
		if key == "" {
			key = RandomSlug() + RandomSlug()
			if _, err := t.ExecContext(ctx,
				`update posts set translation_key=? where id=?`, key, ofID); err != nil {
				return err
			}
		}
		_, err := t.ExecContext(ctx, `update posts set translation_key=? where id=?`, key, id)
		return err
	})
}

// Translations 返回同一译文分组里的其它已发布版本。
//
// 只返回已发布的：hreflang 指向一个 404 或草稿页，比没有 hreflang 更糟——
// Google 会认为整组标注不可信而整体忽略。
func (d *DB) Translations(ctx context.Context, key string, exclude int64) ([]Post, error) {
	if key == "" {
		return nil, nil
	}
	rows, err := d.R.QueryContext(ctx,
		`select `+postColsList+postFrom+`
		  where p.translation_key=? and p.id<>? and p.status=?
		  order by p.lang`, key, exclude, StatusPublished)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// CountByLang 返回各语言的已发布文章数，用于决定要不要显示语言切换器。
func (d *DB) CountByLang(ctx context.Context) (map[string]int, error) {
	rows, err := d.R.QueryContext(ctx,
		`select lang, count(*) from posts where status=? group by lang`, StatusPublished)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var l string
		var n int
		if err := rows.Scan(&l, &n); err != nil {
			return nil, err
		}
		out[l] = n
	}
	return out, rows.Err()
}

// SetSchedule 设置或清除定时发布时刻。at 为 nil 表示取消定时。
//
// 只对草稿有意义：已发布的文章设了也不会再被发一次，已归档的更不该被
// 自动拉回来。所以这里把范围卡死在 draft，而不是靠调用方自觉。
func (db *DB) SetSchedule(ctx context.Context, a Actor, id int64, at *time.Time) error {
	var v any
	if at != nil {
		v = at.Unix()
	}
	res, err := db.W.ExecContext(ctx,
		`update posts set publish_at=?, updated_at=? where id=? and status='draft'`,
		v, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 不是草稿就当无事发生：调用方是表单保存，不该因为这个报错。
		return nil
	}
	return nil
}

// DuePosts 返回到点该发布的草稿。
func (db *DB) DuePosts(ctx context.Context, now time.Time) ([]Post, error) {
	rows, err := db.R.QueryContext(ctx,
		`select `+postColsList+postFrom+`
		 where p.status='draft' and p.publish_at is not null and p.publish_at<=?
		 order by p.publish_at`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// PublishDue 把所有到点的草稿发出去，返回发布成功的篇数和逐篇的失败。
//
// 身份用文章作者本人，而不是一个"定时器"的空身份：排期是作者在后台
// 做的决定，这里只是替他按下按钮。PublishPost 会校验 canTouch(ownerID)，
// 空身份一律过不去；审计记录也该落在作者名下，actor 字段区分来路。
//
// 单篇失败不中断其余——一篇文章的问题不该让整个队列停摆——但失败必须
// 往外冒。一个悄悄什么都不干的定时任务，和一个没有定时任务是一样的，
// 区别只在于前者还让人以为它在工作。
func (db *DB) PublishDue(ctx context.Context, now time.Time) (int, []error) {
	due, err := db.DuePosts(ctx, now)
	if err != nil {
		return 0, []error{err}
	}
	n := 0
	var errs []error
	for i := range due {
		a := Actor{UserID: due[i].UserID, Kind: "scheduler"}
		// cap 传 0：这一篇的发布是人在排期时就授权过的，不该再去占
		// 当天那份给临时发布留的额度。
		if _, err := db.PublishPost(ctx, a, due[i].ID, 0); err != nil {
			errs = append(errs, fmt.Errorf("post %d: %w", due[i].ID, err))
			continue
		}
		n++
	}
	return n, errs
}

// categoryIDBySlug 把分类 slug 换成 ID。slug 为空或找不到都返回 nil，
// 也就是"未分类"。
//
// 找不到不报错是刻意的：AI 客户端写错一个分类名，不该让整次创建失败。
// 草稿先存下来，人在后台看一眼改掉，比丢掉一篇正文划算。
func categoryIDBySlug(ctx context.Context, t *sql.Tx, slug string) (any, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, nil
	}
	var id int64
	err := t.QueryRowContext(ctx, `select id from categories where slug=?`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return id, nil
}
