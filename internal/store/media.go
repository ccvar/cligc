package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cligc.com/internal/imaging"
)

// 允许上传的类型白名单。不做通用文件托管——一个内容站的上传口
// 如果能放任意文件，很快就会变成别人的图床和马甲下载站。
//
// 刻意不收 SVG：SVG 里可以内嵌 <script>，而 /media 和站点同源，
// 等于给了任何能上传的人一个存储型 XSS。要支持 SVG 就得单独换一个
// 域名来托管用户内容，在此之前不如不收。
var allowedMIME = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// MaxMediaSize 是单个文件的上限。
const MaxMediaSize = 8 << 20 // 8 MiB

// SaveMedia 落盘并登记一个上传文件：先转成 WebP，再按**转换后**的内容
// 哈希去重。
//
// 哈希取转换后的字节，是因为它同时充当文件名和去重键。取原始字节的话，
// 同一张图换个 JPEG 质量重新导出一次就是一条新记录，而那两条最终落盘的
// WebP 可能一模一样。
//
// maxDim 是长边上限，<=0 表示不缩。
func (d *DB) SaveMedia(ctx context.Context, root string, userID int64, filename, mime string, data []byte, maxDim int) (*Media, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty file", ErrInvalidInput)
	}
	if len(data) > MaxMediaSize {
		return nil, fmt.Errorf("%w: file exceeds %d bytes", ErrInvalidInput, MaxMediaSize)
	}
	if _, ok := allowedMIME[mime]; !ok {
		return nil, fmt.Errorf("%w: unsupported type %q", ErrInvalidInput, mime)
	}

	// 转换失败不该让上传失败：Process 的失败模式是"什么都没做"，原图照收。
	// 但解不出来的东西要拦住——MIME 是客户端说的，做不得数，而一个存进
	// 媒体库的 .png 实际上是别的东西，是个货真价实的问题。
	img, err := imaging.Process(data, mime, maxDim)
	if err != nil {
		if errors.Is(err, imaging.ErrTooLarge) {
			return nil, fmt.Errorf("%w: image is too large to process", ErrInvalidInput)
		}
		return nil, fmt.Errorf("%w: not a readable image", ErrInvalidInput)
	}
	data, mime = img.Data, img.MIME
	ext := allowedMIME[mime]

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	// 已存在则直接复用
	if m, err := d.MediaByHash(ctx, hash); err == nil {
		return m, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// 按哈希前两位分目录，避免单目录塞几十万文件
	rel := filepath.Join(hash[:2], hash+ext)
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return nil, err
	}

	name := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if name == "" || name == "." || name == "/" {
		name = hash[:8] + ext
	}
	// 转换过就把扩展名也改掉：一个叫 photo.png 的 WebP 文件，下载下来
	// 双击打不开是小事，贴到别处被当成 PNG 处理是麻烦事。
	if img.Converted {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ext
	}
	n := now()
	res, err := d.W.ExecContext(ctx,
		`insert into media(user_id,sha256,filename,mime,size,path,width,height,created_at)
		 values(?,?,?,?,?,?,?,?,?)`,
		userID, hash, name, mime, len(data), rel, img.Width, img.Height, n)
	if err != nil {
		os.Remove(abs)
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Media{ID: id, UserID: userID, SHA256: hash, Filename: name,
		MIME: mime, Size: int64(len(data)), Path: rel,
		Width: img.Width, Height: img.Height, CreatedAt: ts(n)}, nil
}

const mediaCols = `id,user_id,sha256,filename,mime,size,path,width,height,created_at`

func scanMedia(sc interface{ Scan(...any) error }) (*Media, error) {
	var m Media
	var created int64
	err := sc.Scan(&m.ID, &m.UserID, &m.SHA256, &m.Filename, &m.MIME, &m.Size, &m.Path,
		&m.Width, &m.Height, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt = ts(created)
	return &m, nil
}

// MediaByHash 按内容哈希查已上传文件。
func (d *DB) MediaByHash(ctx context.Context, hash string) (*Media, error) {
	return scanMedia(d.R.QueryRowContext(ctx, `select `+mediaCols+` from media where sha256=?`, hash))
}

// ListMedia 列出某用户最近上传的文件。
func (d *DB) ListMedia(ctx context.Context, userID int64, limit int) ([]Media, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.R.QueryContext(ctx,
		`select `+mediaCols+` from media where user_id=? order by created_at desc limit ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Media
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// MediaExt 返回某 MIME 对应的扩展名，未登记则返回空串。
func MediaExt(mime string) string { return allowedMIME[mime] }

// MediaByID 按 id 取一条上传记录。
func (d *DB) MediaByID(ctx context.Context, id int64) (*Media, error) {
	return scanMedia(d.R.QueryRowContext(ctx, `select `+mediaCols+` from media where id=?`, id))
}

// CountMedia 返回某用户的上传总数（管理员传 0 表示全站）。
func (d *DB) CountMedia(ctx context.Context, userID int64) (int, error) {
	var n int
	var err error
	if userID == 0 {
		err = d.R.QueryRowContext(ctx, `select count(*) from media`).Scan(&n)
	} else {
		err = d.R.QueryRowContext(ctx, `select count(*) from media where user_id=?`, userID).Scan(&n)
	}
	return n, err
}

// ListMediaPage 分页列出上传文件。userID 为 0 表示不限作者（管理员视角）。
func (d *DB) ListMediaPage(ctx context.Context, userID int64, limit, offset int) ([]Media, error) {
	if limit <= 0 || limit > 200 {
		limit = 24
	}
	q := `select ` + mediaCols + ` from media`
	args := []any{}
	if userID != 0 {
		q += ` where user_id=?`
		args = append(args, userID)
	}
	q += ` order by created_at desc limit ? offset ?`
	rows, err := d.R.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Media
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// DeleteMedia 删除一条上传记录及其磁盘文件。
//
// 先删库再删盘：反过来的话，删盘成功而删库失败会留下一条指向不存在文件的
// 记录，界面上看得见却打不开。这个顺序下最坏情况是留一个没人引用的孤儿
// 文件，那是可以扫出来清理的。
//
// 注意这里不检查是否还有文章在引用它——文件是按内容哈希去重的，引用关系
// 存在 Markdown 正文里，可靠地反查需要全表扫描正文。界面上会提示这一点。
func (d *DB) DeleteMedia(ctx context.Context, a Actor, root string, id int64) error {
	m, err := d.MediaByID(ctx, id)
	if err != nil {
		return err
	}
	if !a.canTouch(m.UserID) {
		return ErrForbidden
	}
	res, err := d.W.ExecContext(ctx, `delete from media where id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	// 拼路径前确认它确实落在 root 之下，防止 path 字段被污染后删到别处
	abs := filepath.Join(root, filepath.Clean("/"+m.Path))
	if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		os.Remove(abs)
	}
	return nil
}

// mediaSize 回答"/media/ab/xxxx.webp 这张图多大"，给渲染器写 width/height 用。
//
// 每渲染一张站内图片查一次库。这看着频繁，但渲染只发生在**写**的时候
// （正文 HTML 存在库里），读页面一次都不查。
func (d *DB) mediaSize(src string) (int, int, bool) {
	rel, ok := strings.CutPrefix(src, "/media/")
	if !ok || rel == "" {
		return 0, 0, false
	}
	var w, h int
	err := d.R.QueryRow(`select width,height from media where path=?`, rel).Scan(&w, &h)
	if err != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// BackfillMediaSizes 给还没有像素尺寸的媒体补上宽高，返回补上的条数。
//
// 尺寸这一列是后加的，升级上来的库里存量图片全是 0×0。而正文里的 <img>
// 靠它写 width/height——不补的话，老文章里的图仍然会在加载完的瞬间把下面
// 的正文往下顶，而且跑多少次 rerender 都没用：渲染时查到 0 就当没有。
//
// 只读文件头，不重新编码。转码是有损的，对着已经存进库的图再来一遍，
// 是拿画质换一个这里根本不需要的东西。
func (d *DB) BackfillMediaSizes(ctx context.Context, root string) (int, error) {
	rows, err := d.R.QueryContext(ctx,
		`select id, path from media where width<=0 or height<=0 order by id`)
	if err != nil {
		return 0, err
	}
	type item struct {
		id   int64
		path string
	}
	var todo []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.path); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, it := range todo {
		f, err := os.Open(filepath.Join(root, it.path))
		if err != nil {
			continue // 文件不在了就跳过，一条读不出来不该让整批停下
		}
		w, h, err := imaging.Dimensions(f)
		f.Close()
		if err != nil || w <= 0 || h <= 0 {
			continue
		}
		if _, err := d.W.ExecContext(ctx,
			`update media set width=?, height=? where id=?`,
			w, h, it.id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
