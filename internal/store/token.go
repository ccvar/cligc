package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// TokenPrefix 是明文 token 的固定前缀，方便在日志/代码扫描里识别泄露。
const TokenPrefix = "cligc_"

func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// CreateToken 生成一个新的 API token。
//
// 明文只在这里返回一次，库里只留 sha256。ttl 为 0 表示永不过期。
// scopes 会按 AllScopes 白名单过滤——尤其是防止误发 posts:publish。
func (d *DB) CreateToken(ctx context.Context, userID int64, name string, scopes []string, ttl time.Duration) (string, *Token, error) {
	if name == "" {
		return "", nil, fmt.Errorf("%w: token name required", ErrInvalidInput)
	}
	var clean []string
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if slices.Contains(AllScopes, s) && !slices.Contains(clean, s) {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return "", nil, fmt.Errorf("%w: at least one valid scope required", ErrInvalidInput)
	}

	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	plain := TokenPrefix + hex.EncodeToString(b)
	prefix := plain[:len(TokenPrefix)+6]

	var exp *int64
	if ttl > 0 {
		v := time.Now().Add(ttl).Unix()
		exp = &v
	}
	n := now()
	res, err := d.W.ExecContext(ctx,
		`insert into api_tokens(user_id,name,prefix,token_hash,scopes,created_at,expires_at) values(?,?,?,?,?,?,?)`,
		userID, name, prefix, hashToken(plain), strings.Join(clean, ","), n, exp)
	if err != nil {
		return "", nil, err
	}
	id, _ := res.LastInsertId()
	t := &Token{ID: id, UserID: userID, Name: name, Prefix: prefix, Scopes: clean, CreatedAt: ts(n)}
	if exp != nil {
		e := ts(*exp)
		t.ExpiresAt = &e
	}
	return plain, t, nil
}

// AuthenticateToken 用明文 token 换取 token 元数据与所属用户。
// 已吊销或已过期的 token 一律返回 ErrUnauthorized。
func (d *DB) AuthenticateToken(ctx context.Context, plain string) (*Token, *User, error) {
	if !strings.HasPrefix(plain, TokenPrefix) {
		return nil, nil, ErrUnauthorized
	}
	var (
		t        Token
		scopes   string
		created  int64
		lastUsed sql.NullInt64
		expires  sql.NullInt64
		revoked  sql.NullInt64
	)
	err := d.R.QueryRowContext(ctx,
		`select id,user_id,name,prefix,scopes,created_at,last_used_at,expires_at,revoked_at
		   from api_tokens where token_hash=?`, hashToken(plain)).
		Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &scopes, &created, &lastUsed, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrUnauthorized
	}
	if err != nil {
		return nil, nil, err
	}
	if revoked.Valid {
		return nil, nil, fmt.Errorf("%w: token revoked", ErrUnauthorized)
	}
	if expires.Valid && expires.Int64 < now() {
		return nil, nil, fmt.Errorf("%w: token expired", ErrUnauthorized)
	}

	t.Scopes = splitScopes(scopes)
	t.CreatedAt = ts(created)
	t.LastUsedAt = nullTime(lastUsed)
	t.ExpiresAt = nullTime(expires)

	// last_used_at 每次请求都写会把读多写少的结构毁掉，节流到 60 秒一次。
	if !lastUsed.Valid || now()-lastUsed.Int64 > 60 {
		d.W.ExecContext(ctx, `update api_tokens set last_used_at=? where id=?`, now(), t.ID)
	}

	u, err := d.UserByID(ctx, t.UserID)
	if err != nil {
		return nil, nil, err
	}
	return &t, u, nil
}

// ListTokens 列出某用户的全部 token（含已吊销的，便于审计）。
func (d *DB) ListTokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := d.R.QueryContext(ctx,
		`select id,user_id,name,prefix,scopes,created_at,last_used_at,expires_at,revoked_at
		   from api_tokens where user_id=? order by created_at desc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var (
			t                        Token
			scopes                   string
			created                  int64
			lastUsed, expires, revok sql.NullInt64
		)
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &scopes, &created, &lastUsed, &expires, &revok); err != nil {
			return nil, err
		}
		t.Scopes = splitScopes(scopes)
		t.CreatedAt = ts(created)
		t.LastUsedAt = nullTime(lastUsed)
		t.ExpiresAt = nullTime(expires)
		t.RevokedAt = nullTime(revok)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken 吊销 token。带 userID 条件，防止越权吊销他人 token。
func (d *DB) RevokeToken(ctx context.Context, userID, id int64) error {
	res, err := d.W.ExecContext(ctx,
		`update api_tokens set revoked_at=? where id=? and user_id=? and revoked_at is null`,
		now(), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateTokenAs 代表另一个 token 签发新 token，并且不允许提权。
//
// 这是把"签发 token"开放给 API 的前提。没有这条约束的话，一个只有
// posts:write 的 token 可以给自己签一个带 posts:publish 的——"AI 只能写
// 草稿"这道闸门就成了摆设，而且绕过它只需要一次 API 调用。
//
// parent 为 nil 表示调用方是人（网页后台），不受此限。
func (d *DB) CreateTokenAs(ctx context.Context, parent []string, userID int64,
	name string, scopes []string, ttl time.Duration) (string, *Token, error) {
	if parent != nil {
		for _, want := range scopes {
			if !slices.Contains(parent, want) {
				return "", nil, fmt.Errorf("%w: 不能签发超出自身权限的 %s", ErrForbidden, want)
			}
		}
	}
	return d.CreateToken(ctx, userID, name, scopes, ttl)
}
