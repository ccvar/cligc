package store

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// pbkdf2 迭代次数。用标准库的 crypto/pbkdf2（Go 1.24+）而不是 x/crypto/bcrypt，
// 是为了守住"零外部依赖做认证"这条线。
const pbkdf2Iter = 210_000

// HashPassword 生成 "pbkdf2$iter$salt$key" 格式的密码哈希。
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 校验密码，使用恒定时间比较。
func VerifyPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// CreateUser 新建用户。email 唯一，slug 由 name 推导并保证唯一。
func (d *DB) CreateUser(ctx context.Context, email, name, password, role string) (*User, error) {
	if email == "" || name == "" || len(password) < 8 {
		return nil, fmt.Errorf("%w: email/name required, password >= 8 chars", ErrInvalidInput)
	}
	if role != "admin" && role != "author" {
		role = "author"
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	email = strings.ToLower(strings.TrimSpace(email))

	var u User
	err = d.tx(ctx, func(t *sql.Tx) error {
		slug, err := uniqueSlug(ctx, t, "users", Slugify(name), 0)
		if err != nil {
			return err
		}
		n := now()
		res, err := t.ExecContext(ctx,
			`insert into users(email,name,slug,password_hash,role,created_at) values(?,?,?,?,?,?)`,
			email, name, slug, hash, role, n)
		if err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: email already registered", ErrConflict)
			}
			return err
		}
		id, _ := res.LastInsertId()
		u = User{ID: id, Email: email, Name: name, Slug: slug, Role: role, CreatedAt: ts(n)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &u, nil
}

const userCols = `id,email,name,slug,bio,role,created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created int64
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Slug, &u.Bio, &u.Role, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = ts(created)
	return &u, nil
}

// UserByID 按 id 取用户。
func (d *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(d.R.QueryRowContext(ctx, `select `+userCols+` from users where id=?`, id))
}

// UserBySlug 按 slug 取用户。
func (d *DB) UserBySlug(ctx context.Context, slug string) (*User, error) {
	return scanUser(d.R.QueryRowContext(ctx, `select `+userCols+` from users where slug=?`, slug))
}

// Authenticate 用邮箱密码验证身份。无论用户是否存在都走一次 KDF，
// 避免通过响应时间区分"用户不存在"和"密码错误"。
func (d *DB) Authenticate(ctx context.Context, email, password string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var hash string
	var id int64
	err := d.R.QueryRowContext(ctx, `select id,password_hash from users where email=?`, email).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// 用一个固定的假哈希消耗等量时间
		VerifyPassword("pbkdf2$210000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if !VerifyPassword(hash, password) {
		return nil, ErrUnauthorized
	}
	return d.UserByID(ctx, id)
}

// --- 网页会话 ---

const sessionTTL = 30 * 24 * time.Hour

// CreateSession 新建一个网页登录会话，返回 cookie 用的 session id。
func (d *DB) CreateSession(ctx context.Context, userID int64) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	id := hex.EncodeToString(b)
	exp := time.Now().Add(sessionTTL)
	_, err := d.W.ExecContext(ctx,
		`insert into sessions(id,user_id,created_at,expires_at) values(?,?,?,?)`,
		id, userID, now(), exp.Unix())
	if err != nil {
		return "", time.Time{}, err
	}
	return id, exp, nil
}

// UserBySession 用 session id 取用户；过期或不存在返回 ErrNotFound。
func (d *DB) UserBySession(ctx context.Context, sid string) (*User, error) {
	if sid == "" {
		return nil, ErrNotFound
	}
	var uid int64
	err := d.R.QueryRowContext(ctx,
		`select user_id from sessions where id=? and expires_at > ?`, sid, now()).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d.UserByID(ctx, uid)
}

// DeleteSession 注销一个会话。
func (d *DB) DeleteSession(ctx context.Context, sid string) error {
	_, err := d.W.ExecContext(ctx, `delete from sessions where id=?`, sid)
	return err
}

// CountUsers 返回用户总数，用于首次启动时判断是否需要引导建管理员。
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `select count(*) from users`).Scan(&n)
	return n, err
}

// --- 内部工具 ---

// uniqueSlug 在 table 里找一个没被占用的 slug，必要时追加 -2、-3……
// excludeID 用于更新自身时跳过自己那一行。
func uniqueSlug(ctx context.Context, t *sql.Tx, table, base string, excludeID int64) (string, error) {
	if base == "" {
		base = RandomSlug()
	}
	q := `select count(*) from ` + table + ` where slug=? and id<>?`
	cand := base
	for i := 2; i < 200; i++ {
		var n int
		if err := t.QueryRowContext(ctx, q, cand, excludeID).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return cand, nil
		}
		cand = fmt.Sprintf("%s-%d", base, i)
	}
	return base + "-" + RandomSlug(), nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// UpdateUser 修改资料。slug 变化时保证唯一。
//
// 不允许改邮箱：邮箱是登录标识，改它等于换账号，而且需要一轮验证流程
// 才能防止误填后自己锁死。真要换就新建账号再转移文章。
func (d *DB) UpdateUser(ctx context.Context, id int64, name, bio, slug string) (*User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: name required", ErrInvalidInput)
	}
	if len([]rune(bio)) > 500 {
		return nil, fmt.Errorf("%w: bio too long (max 500 chars)", ErrInvalidInput)
	}
	err := d.tx(ctx, func(t *sql.Tx) error {
		base := Slugify(slug)
		if strings.TrimSpace(slug) == "" {
			base = Slugify(name)
		}
		s, err := uniqueSlug(ctx, t, "users", base, id)
		if err != nil {
			return err
		}
		res, err := t.ExecContext(ctx,
			`update users set name=?, bio=?, slug=? where id=?`, name, bio, s, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d.UserByID(ctx, id)
}

// ChangePassword 修改密码，需要先验证旧密码。
//
// 改密成功后吊销该用户的其它所有会话——改密码最常见的动机就是"怀疑被盗号"，
// 如果攻击者的会话还留着，改密码就白改了。当前会话由调用方负责重建。
func (d *DB) ChangePassword(ctx context.Context, id int64, oldPw, newPw string) error {
	if len(newPw) < 8 {
		return fmt.Errorf("%w: password must be at least 8 characters", ErrInvalidInput)
	}
	var hash string
	if err := d.R.QueryRowContext(ctx, `select password_hash from users where id=?`, id).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !VerifyPassword(hash, oldPw) {
		return fmt.Errorf("%w: current password is incorrect", ErrUnauthorized)
	}
	next, err := HashPassword(newPw)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(t *sql.Tx) error {
		if _, err := t.ExecContext(ctx, `update users set password_hash=? where id=?`, next, id); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `delete from sessions where user_id=?`, id)
		return err
	})
}
