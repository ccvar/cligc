package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Category 是站点的板块。一篇文章最多属于一个。
//
// 和 Tag 的区别不在实现而在用途：分类回答"这个站分几块"，标签回答
// "这篇讲了什么"。前者要少、要稳、要能排序；后者可以随手加、无所谓顺序。
type Category struct {
	ID   int64
	Slug string
	// Name / Desc 是**按请求的语言解析过**的那一份。
	Name string
	Desc string
	Sort int
	// Count 是当前语言下的已发布篇数。
	Count int

	// Names / Descs 是各语言的原始值，key 是语言代码，只在后台填充。
	// 公开页拿到的是解析后的 Name，不需要知道还有哪些语言。
	Names map[string]string
	Descs map[string]string
}

// CategoryInput 是新建或修改一个板块要给的全部内容。
//
// 用结构体而不是一串位置参数：加上按语言的名字之后参数已经有六个，
// 其中两个还是同类型的 map——调用方把它们写反了编译器不会说话。
type CategoryInput struct {
	Slug string
	Sort int
	// Names / Descs 的 key 是语言代码。DefaultLang 那一份存进 categories
	// 表本身，其余的进 category_i18n。
	Names       map[string]string
	Descs       map[string]string
	DefaultLang string
}

// name 取默认语言那一份，它是必填的。
func (in CategoryInput) name() string { return strings.TrimSpace(in.Names[in.DefaultLang]) }
func (in CategoryInput) desc() string { return strings.TrimSpace(in.Descs[in.DefaultLang]) }

// 按 lang 解析名字的取数。列清单和 from 分开写，是因为列表查询要在
// 中间再插一列"当前语言下的篇数"——拼在 from 后面是语法错误，而那种
// 错误的表现是列表静悄悄地变空，不会有人看见报错。
//
// nullif 那一层：译名的行存在但名字留空，等于没翻，该退回默认语言
// 那一份，而不是显示一个空标题。
const (
	categoryCols = `c.id, c.slug,
	       coalesce(nullif(t.name,''), c.name),
	       coalesce(nullif(t.description,''), c.description),
	       c.sort`
	categoryFrom = ` from categories c
	  left join category_i18n t on t.category_id=c.id and t.lang=?`
)

// ListCategories 按导航顺序返回全部板块，名字按 lang 解析，
// Count 是**该语言下**的已发布篇数。
//
// 只数已发布的：侧栏上写着"随笔 3"，点进去却只有 1 篇（另外 2 篇是草稿），
// 这种数字比不显示更糟。按语言数则是因为列表本身就是按语言过滤的——
// 全站口径的数字会让英文站的侧栏写着"随笔 9"，点进去一篇都没有。
func (d *DB) ListCategories(ctx context.Context, lang string) ([]Category, error) {
	rows, err := d.R.QueryContext(ctx,
		`select `+categoryCols+`,
	       (select count(*) from posts p
	         where p.category_id=c.id and p.status=? and p.lang=?)`+
			categoryFrom+` order by c.sort, c.name`,
		StatusPublished, lang, lang)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Desc, &c.Sort, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCategoriesAdmin 给后台用：带上每种语言的原始名字，Count 是全站
// 所有语言的合计。
//
// 后台要的是"这个板块底下一共有多少东西"，不是"当前界面语言下有多少"——
// 界面语言是站长的个人偏好，和内容分布没有关系。
func (d *DB) ListCategoriesAdmin(ctx context.Context, defaultLang string) ([]Category, error) {
	rows, err := d.R.QueryContext(ctx, `
		select c.id, c.slug, c.name, c.description, c.sort,
		       (select count(*) from posts p where p.category_id=c.id and p.status=?)
		  from categories c
		 order by c.sort, c.name`, StatusPublished)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]int{}
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Desc, &c.Sort, &c.Count); err != nil {
			return nil, err
		}
		c.Names = map[string]string{defaultLang: c.Name}
		c.Descs = map[string]string{defaultLang: c.Desc}
		byID[c.ID] = len(out)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	tr, err := d.R.QueryContext(ctx,
		`select category_id, lang, name, description from category_i18n`)
	if err != nil {
		return nil, err
	}
	defer tr.Close()
	for tr.Next() {
		var id int64
		var lang, name, desc string
		if err := tr.Scan(&id, &lang, &name, &desc); err != nil {
			return nil, err
		}
		if i, ok := byID[id]; ok && lang != defaultLang {
			out[i].Names[lang] = name
			out[i].Descs[lang] = desc
		}
	}
	return out, tr.Err()
}

// CategoryBySlug 按 slug 取一个板块，名字按 lang 解析。
func (d *DB) CategoryBySlug(ctx context.Context, slug, lang string) (*Category, error) {
	var c Category
	err := d.R.QueryRowContext(ctx,
		`select `+categoryCols+categoryFrom+` where c.slug=?`, lang, slug).
		Scan(&c.ID, &c.Slug, &c.Name, &c.Desc, &c.Sort)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// CreateCategory 新建一个板块。slug 留空按默认语言的名字生成。
func (d *DB) CreateCategory(ctx context.Context, in CategoryInput) (*Category, error) {
	name := in.name()
	if name == "" {
		return nil, fmt.Errorf("%w: name required", ErrInvalidInput)
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = TagSlug(name)
	}
	var id int64
	err := d.tx(ctx, func(t *sql.Tx) error {
		res, err := t.ExecContext(ctx,
			`insert into categories(slug,name,description,sort) values(?,?,?,?)`,
			slug, name, in.desc(), in.Sort)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return writeCategoryI18n(ctx, t, id, in)
	})
	if err != nil {
		return nil, err
	}
	return &Category{ID: id, Slug: slug, Name: name, Desc: in.desc(), Sort: in.Sort}, nil
}

// UpdateCategory 改名（含各语言的译名）、改 slug、改顺序。
func (d *DB) UpdateCategory(ctx context.Context, id int64, in CategoryInput) error {
	name := in.name()
	if name == "" {
		return fmt.Errorf("%w: name required", ErrInvalidInput)
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = TagSlug(name)
	}
	return d.tx(ctx, func(t *sql.Tx) error {
		if _, err := t.ExecContext(ctx,
			`update categories set name=?, slug=?, description=?, sort=? where id=?`,
			name, slug, in.desc(), in.Sort, id); err != nil {
			return err
		}
		return writeCategoryI18n(ctx, t, id, in)
	})
}

// writeCategoryI18n 先清后写这个板块的所有译名。
//
// 和站名那边一样是动态 key：条数随语言数变，逐条 upsert 的话，把一个
// 译名清空之后那一行还留在库里，下次读出来就是一个没人记得设过的旧名字。
// 只写非默认语言——默认语言那一份在 categories 表本身上。
func writeCategoryI18n(ctx context.Context, t *sql.Tx, id int64, in CategoryInput) error {
	if _, err := t.ExecContext(ctx, `delete from category_i18n where category_id=?`, id); err != nil {
		return err
	}
	for lang, name := range in.Names {
		if lang == in.DefaultLang || lang == "" {
			continue
		}
		name = strings.TrimSpace(name)
		desc := strings.TrimSpace(in.Descs[lang])
		if name == "" && desc == "" {
			continue
		}
		if _, err := t.ExecContext(ctx,
			`insert into category_i18n(category_id,lang,name,description) values(?,?,?,?)`,
			id, lang, name, desc); err != nil {
			return err
		}
	}
	// 只填了描述没填名字的语言，上面那个循环走不到（Names 里没有这个 key）
	for lang, desc := range in.Descs {
		if lang == in.DefaultLang || lang == "" || in.Names[lang] != "" {
			continue
		}
		if desc = strings.TrimSpace(desc); desc == "" {
			continue
		}
		if _, err := t.ExecContext(ctx,
			`insert or replace into category_i18n(category_id,lang,name,description)
			 values(?,?,'',?)`, id, lang, desc); err != nil {
			return err
		}
	}
	return nil
}

// DeleteCategory 删除一个板块。
//
// 文章不跟着删——posts.category_id 是 on delete set null，属于这个板块的
// 文章会变成"未分类"，内容一篇不少。删板块是整理导航，不是删内容，
// 这两件事的后果差太远，不能因为一次误点就混在一起。
// 译名跟着删（on delete cascade）：它们只对这个板块有意义。
func (d *DB) DeleteCategory(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, `delete from categories where id=?`, id)
	return err
}

// ReorderCategories 按给定的 ID 顺序重排。
//
// 拖动排序会一次调整好几项的相对位置，逐条 update 的话中间任何一步失败
// 都会留下一个半新半旧的顺序。整体放进一个事务，要么全成要么全不动。
func (d *DB) ReorderCategories(ctx context.Context, ids []int64) error {
	return d.tx(ctx, func(t *sql.Tx) error {
		for i, id := range ids {
			if _, err := t.ExecContext(ctx,
				`update categories set sort=? where id=?`, i, id); err != nil {
				return err
			}
		}
		return nil
	})
}
