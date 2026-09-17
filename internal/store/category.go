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
	ID    int64
	Slug  string
	Name  string
	Sort  int
	Count int // 已发布文章数，列表页用
}

// ListCategories 按导航顺序返回全部分类，附带已发布文章数。
//
// 只数已发布的：侧栏和导航上写着"随笔 3"，点进去却只有 1 篇（另外 2 篇
// 是草稿），这种数字比不显示更糟。
func (d *DB) ListCategories(ctx context.Context) ([]Category, error) {
	rows, err := d.R.QueryContext(ctx, `
		select c.id, c.slug, c.name, c.sort,
		       (select count(*) from posts p where p.category_id=c.id and p.status='published')
		  from categories c
		 order by c.sort, c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Sort, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CategoryBySlug 按 slug 取一个分类。
func (d *DB) CategoryBySlug(ctx context.Context, slug string) (*Category, error) {
	var c Category
	err := d.R.QueryRowContext(ctx,
		`select id, slug, name, sort from categories where slug=?`, slug).
		Scan(&c.ID, &c.Slug, &c.Name, &c.Sort)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// CreateCategory 新建一个分类。slug 留空按 name 生成。
func (d *DB) CreateCategory(ctx context.Context, name, slug string, sort int) (*Category, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: name required", ErrInvalidInput)
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = TagSlug(name)
	}
	res, err := d.W.ExecContext(ctx,
		`insert into categories(slug,name,sort) values(?,?,?)`, slug, name, sort)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Category{ID: id, Slug: slug, Name: name, Sort: sort}, nil
}

// UpdateCategory 改名、改 slug、改顺序。
func (d *DB) UpdateCategory(ctx context.Context, id int64, name, slug string, sort int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: name required", ErrInvalidInput)
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = TagSlug(name)
	}
	_, err := d.W.ExecContext(ctx,
		`update categories set name=?, slug=?, sort=? where id=?`, name, slug, sort, id)
	return err
}

// DeleteCategory 删除一个分类。
//
// 文章不跟着删——posts.category_id 是 on delete set null，属于这个分类的
// 文章会变成"未分类"，内容一篇不少。删分类是整理导航，不是删内容，
// 这两件事的后果差太远，不能因为一次误点就混在一起。
func (d *DB) DeleteCategory(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, `delete from categories where id=?`, id)
	return err
}
