package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"cligc.com/internal/store"
)

type ctxKey int

const (
	ctxToken ctxKey = iota
	ctxUser
)

// auth 校验 Bearer token 并检查 scope。
//
// scope 检查放在路由注册处（见 Routes）而不是 handler 内部，好处是
// "哪个接口需要什么权限"可以在一屏里看完 —— 权限漏配是这类系统最常见的
// 安全缺陷，把它集中到一处才看得住。
func (s *Server) auth(scope string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("Authorization")
		if !strings.HasPrefix(raw, "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cligc"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{
				Error: "missing bearer token", Code: "unauthorized"})
			return
		}
		plain := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))

		tok, user, err := s.db.AuthenticateToken(r.Context(), plain)
		if err != nil {
			fail(w, err)
			return
		}
		if !tok.Has(scope) {
			writeJSON(w, http.StatusForbidden, errorBody{
				Error:   "token lacks required scope",
				Code:    "missing_scope",
				Details: "required: " + scope + "; granted: " + strings.Join(tok.Scopes, ","),
			})
			return
		}

		// 写操作限速：防止 AI 客户端进入循环后在站上刷出几千篇草稿。
		// 每日发布上限管的是"发出去多少"，这里管的是"写进来多快"。
		if r.Method != http.MethodGet && !s.lim.allow(tok.ID) {
			writeJSON(w, http.StatusTooManyRequests, errorBody{
				Error: "too many write requests", Code: "rate_limited"})
			return
		}

		ctx := context.WithValue(r.Context(), ctxToken, tok)
		ctx = context.WithValue(ctx, ctxUser, user)
		next(w, r.WithContext(ctx))
	})
}

func tokenFrom(ctx context.Context) *store.Token {
	v, _ := ctx.Value(ctxToken).(*store.Token)
	return v
}
func userFrom(ctx context.Context) *store.User { v, _ := ctx.Value(ctxUser).(*store.User); return v }

// actor 从请求上下文构造一个 store.Actor，Kind 固定为 api ——
// 发布事件里留下这个标记，事后才分得清哪些内容是从 AI 客户端发出去的。
func actor(ctx context.Context) store.Actor {
	u := userFrom(ctx)
	if u == nil {
		return store.Actor{Kind: "api"}
	}
	return store.Actor{UserID: u.ID, IsAdmin: u.IsAdmin(), Kind: "api"}
}

// limiter 是一个按 token 计数的固定窗口限速器。
//
// 用固定窗口而不是令牌桶，是因为这里只需要拦住失控的循环，不需要平滑整形；
// 单机单进程，内存里一张 map 就够，不值得为它引入 Redis。
type limiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	hits   map[int64]*counter
}

type counter struct {
	n     int
	reset time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{window: window, max: max, hits: map[int64]*counter{}}
}

func (l *limiter) allow(key int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	c, ok := l.hits[key]
	if !ok || now.After(c.reset) {
		// 顺带清掉过期条目，避免 map 无限增长
		if len(l.hits) > 1024 {
			for k, v := range l.hits {
				if now.After(v.reset) {
					delete(l.hits, k)
				}
			}
		}
		l.hits[key] = &counter{n: 1, reset: now.Add(l.window)}
		return true
	}
	if c.n >= l.max {
		return false
	}
	c.n++
	return true
}
