// Package store 封装 SQLite 存取。
//
// 这里有两个和"把 SQLite 用对"直接相关的决定，值得单独说明：
//
//  1. 读写分离成两个 *sql.DB。SQLite 是单写者模型：任意时刻只允许一个写事务。
//     写池固定 MaxOpenConns(1)，把排队交给 Go 的连接池而不是让 SQLite 抛
//     "database is locked"；读池则可以开多个连接并发跑。
//
//  2. 必须开 WAL。默认的 rollback journal 模式下读写互相阻塞，一个慢查询就能
//     卡住写入——大多数"SQLite 不行"的结论都来自没开 WAL。
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"cligc.com/internal/render"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// DB 持有读写两个连接池。并发安全。
type DB struct {
	W    *sql.DB // 写池：MaxOpenConns(1)
	R    *sql.DB // 读池：可并发
	rend *render.Renderer

	// settings 是站点设置的内存快照。它在每个页面的 <head> 里都要用到，
	// 每次渲染回库查一遍没有意义。
	//
	// 挂在 DB 上而不是包级变量：同一个进程里可能有多个 DB 实例（测试就是
	// 这样），包级缓存会让它们互相读到对方的设置。
	settings atomic.Pointer[SiteSettings]

	// OnPostChanged 在一篇文章的对外可见性可能变化之后被调用：发布、
	// 改内容、撤下、归档、删除。用来通知 IndexNow 之类的外部服务。
	//
	// 挂在 store 而不是各个 handler 上，是因为改动来自三条不同的路径
	// （后台表单、API、定时任务）。挂在 handler 上就要在三处各记一次，
	// 而漏掉的那条路径不会报错——只是那些文章永远不会被提交。
	//
	// 实现必须是非阻塞的：它在写事务之后同步调用，卡住它就是卡住发布。
	OnPostChanged func(p *Post)
}

// notifyChanged 在 OnPostChanged 挂了钩子时调用它。p 为 nil 时跳过。
func (d *DB) notifyChanged(p *Post) {
	if d.OnPostChanged != nil && p != nil {
		d.OnPostChanged(p)
	}
}

// dsn 拼出 modernc.org/sqlite 的连接串。_pragma 参数对每条新建连接生效，
// 这正是我们要的——连接池里任何一条连接都带着同样的设置。
func dsn(path string, readonly bool) string {
	q := url.Values{}
	add := func(v string) { q.Add("_pragma", v) }
	add("journal_mode(WAL)")   // 读写不互相阻塞
	add("busy_timeout(5000)")  // 拿不到锁时等待而不是立刻报错
	add("foreign_keys(1)")     // SQLite 默认不开外键约束
	add("synchronous(NORMAL)") // WAL 下的推荐值：崩溃不丢已提交事务
	if readonly {
		add("query_only(1)")
	} else {
		add("wal_autocheckpoint(1000)")
	}
	return "file:" + path + "?" + q.Encode()
}

// Open 打开（必要时创建）数据库并跑建表语句。siteHost 传给渲染器用于识别站外链接。
func Open(path, siteHost string) (*DB, error) {
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	if _, err := w.Exec(schemaSQL); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := migrate(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	r.SetMaxOpenConns(8)
	r.SetMaxIdleConns(8)
	r.SetConnMaxLifetime(0)

	return &DB{W: w, R: r, rend: render.New(siteHost)}, nil
}

// migrate 补上 schema.sql 里的 create table 覆盖不到的变更。
//
// schema.sql 全是 create ... if not exists，对全新的库是完整的，但对已经
// 存在的库，新加的列不会被套用——建表语句直接跳过了。加列这类纯增量的
// 改动放在这里。
//
// 刻意只支持"加列"这一种操作，不做通用迁移框架：真需要改列类型或删列时，
// SQLite 本来就得走"建新表-搬数据-换名"那一套，那种改动值得单独写清楚，
// 不该藏在一个看起来什么都能干的助手函数里。
func migrate(db *sql.DB) error {
	// 第一阶段：补列。
	type col struct{ table, name, ddl string }
	for _, c := range []col{
		{"posts", "featured_at", "integer"},
		{"posts", "toc_json", "text not null default ''"},
		{"posts", "lang", "text not null default 'zh-Hans'"},
		{"posts", "translation_key", "text not null default ''"},
		{"posts", "publish_at", "integer"},
		{"posts", "category_id", "integer references categories(id) on delete set null"},
	} {
		has, err := hasColumn(db, c.table, c.name)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec("alter table " + c.table + " add column " + c.name + " " + c.ddl); err != nil {
			return fmt.Errorf("add %s.%s: %w", c.table, c.name, err)
		}
	}

	// 第二阶段：建依赖上面那些列的索引。
	//
	// 必须分开两阶段：这类索引不能写进 schema.sql，因为那个文件整体跑在
	// 补列之前——在旧库上 create index 会因为列还不存在而失败，
	// 并且连带把后面的 alter table 一起挡掉，结果是整个库卡在半路。
	for _, idx := range []string{
		`create index if not exists idx_posts_feat on posts(featured_at desc) where featured_at is not null`,
		`create index if not exists idx_posts_lang on posts(lang, status, published_at desc)`,
		`create index if not exists idx_posts_trans on posts(translation_key) where translation_key <> ''`,
		`create index if not exists idx_posts_sched on posts(publish_at) where publish_at is not null`,
		`create index if not exists idx_posts_cat on posts(category_id, status, published_at desc)`,
	} {
		if _, err := db.Exec(idx); err != nil {
			return fmt.Errorf("index: %w", err)
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, name string) (bool, error) {
	rows, err := db.Query("select name from pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
		if n == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close 关闭两个池。
func (d *DB) Close() error {
	e1 := d.R.Close()
	e2 := d.W.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

// tx 在写池上跑一个事务，失败自动回滚。
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		t.Rollback()
		return err
	}
	return t.Commit()
}

// Vacuum 做一次定期清理：过期会话、过期幂等键。
// 幂等键保留 24 小时——足够覆盖 AI 客户端的重试窗口，又不会无限增长。
func (d *DB) Vacuum(ctx context.Context) error {
	now := time.Now().Unix()
	return d.tx(ctx, func(t *sql.Tx) error {
		if _, err := t.ExecContext(ctx, `delete from sessions where expires_at < ?`, now); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `delete from idempotency where created_at < ?`, now-86400)
		return err
	})
}

// --- 小工具 ---

func now() int64 { return time.Now().Unix() }

func ts(v int64) time.Time { return time.Unix(v, 0).UTC() }

// nullInt 是 sql.NullInt64 的短别名，只为让 scan 处的字段声明短一点。
type nullInt = sql.NullInt64

func nullTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := ts(v.Int64)
	return &t
}

func today() string { return time.Now().UTC().Format("2006-01-02") }

// splitScopes 把逗号分隔的 scope 串切成集合。
func splitScopes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
