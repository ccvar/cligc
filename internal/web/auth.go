package web

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"cligc.com/internal/store"
)

const sessionCookie = "cligc_session"

// 登录尝试的限速窗口。API token 那一侧有按 token 的限速，但登录接口
// 是匿名的——没有这层，密码就是可以慢速爆破的。
const (
	loginMaxAttempts = 8
	loginWindow      = 10 * time.Minute
)

// attemptLimiter 是一个固定窗口计数器，按任意字符串键限速。
//
// 同时按 IP 和按邮箱两个维度计数：只按 IP 挡不住分布式撞库，
// 只按邮箱挡不住针对多个账号的横向扫描，两个都要。按邮箱那一维
// 还有个额外好处——它不受反向代理和 X-Forwarded-For 配置影响。
type attemptLimiter struct {
	mu   sync.Mutex
	hits map[string]*window
}

type window struct {
	n     int
	reset time.Time
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{hits: map[string]*window{}}
}

// allow 报告该键是否还在额度内，并计数。max 由调用方给：
// 登录和评论共用这一个计数器，但额度不同——登录 8 次，评论 3 条。
// 额度写死在这里的话，评论会悄悄沿用登录的 8 次，和提示语对不上。
func (l *attemptLimiter) allow(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w, ok := l.hits[key]
	if !ok || now.After(w.reset) {
		if len(l.hits) > 4096 {
			for k, v := range l.hits {
				if now.After(v.reset) {
					delete(l.hits, k)
				}
			}
		}
		l.hits[key] = &window{n: 1, reset: now.Add(loginWindow)}
		return true
	}
	if w.n >= max {
		return false
	}
	w.n++
	return true
}

// reset 在登录成功后清掉计数，避免正常用户偶尔输错后被自己的历史拖累。
func (l *attemptLimiter) reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.hits, k)
	}
}

// clientIP 取请求来源地址。
//
// 只有在 -trust-proxy 打开时才看 X-Forwarded-For，并取最右一项——
// 那是我们自己的反向代理观察到的直连地址。无条件信任这个头等于
// 让任何人自选限速桶，那比不限速还糟。
// rateKey 把来源 IP 归一成限速用的键。
//
// IPv6 必须按 /64 聚合。运营商给一条家宽分配的就是一个 /64，里面有 2^64 个
// 地址，攻击者换一个地址就重置一次计数——按完整地址限速等于没有限速。
// 实测过：同一个 /64 里换 40 个地址提交，40 条全进；换成同一个 IPv4 地址
// 提交 20 次，只进 3 条。
//
// IPv4 保持按完整地址：/24 之类的聚合会把同一个 NAT 出口后面的真实用户
// 一起误伤，而 IPv4 地址本身就稀缺，攻击者拿不到成片的地址。
func rateKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip // 解析不了就原样用，至少不比现在差
	}
	if addr.Is4() || addr.Is4In6() {
		return addr.Unmap().String()
	}
	pre, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return pre.String()
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type ctxKey int

const ctxUser ctxKey = iota

func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxUser).(*store.User)
	return u
}

// WithSession 是全局中间件：解析会话 cookie，把用户放进 context。
// 它不拒绝任何请求——公开页面也需要知道"当前是谁"才能显示后台入口，
// 以及让作者预览自己的草稿。
func (s *Server) WithSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if u, err := s.db.UserBySession(r.Context(), c.Value); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxUser, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireLogin 包住后台路由：未登录跳转到登录页，并对写操作做同源检查。
func (s *Server) requireLogin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r.Context())
		if u == nil {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet && !sameOrigin(r) {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// sameOrigin 做 CSRF 防护。
//
// 没有用传统的 CSRF token：会话 cookie 已经是 SameSite=Lax，跨站 POST
// 本来就带不上 cookie；这里的检查是第二道防线，覆盖 Lax 的边界情况。
// 现代浏览器都会发 Sec-Fetch-Site，优先用它；退化到比对 Origin。
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "cross-site", "same-site":
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// 没有 Origin 的非 GET 请求基本只来自非浏览器客户端（curl 等），
		// 它们拿不到用户的 cookie，不构成 CSRF。
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if userFrom(r.Context()) != nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login.html", page{
		Title: s.tr(r, "login.title"), NoIndex: true,
		Data: map[string]any{"Next": r.URL.Query().Get("next")},
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, s.tr(r, "err.badForm"))
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	// 同样走 rateKey：登录这一侧的 IPv6 漏洞比评论那边更严重——
	// 那是撞库，换个地址就重置计数等于八次尝试的上限完全不存在。
	ipKey, mailKey := "ip:"+rateKey(clientIP(r, s.cfg.TrustProxy)), "email:"+email
	if !s.login.allow(ipKey, loginMaxAttempts) || !s.login.allow(mailKey, loginMaxAttempts) {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, r, "login.html", page{
			Title: s.tr(r, "login.title"), NoIndex: true,
			Flash: s.tr(r, "login.tooMany"),
			Data:  map[string]any{"Next": r.FormValue("next"), "Email": r.FormValue("email")},
		})
		return
	}

	u, err := s.db.Authenticate(r.Context(), email, r.FormValue("password"))
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login.html", page{
			Title: s.tr(r, "login.title"), NoIndex: true,
			Flash: s.tr(r, "login.failed"),
			Data:  map[string]any{"Next": r.FormValue("next"), "Email": r.FormValue("email")},
		})
		return
	}
	s.login.reset(ipKey, mailKey)
	sid, exp, err := s.db.CreateSession(r.Context(), u.ID)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, s.tr(r, "err.internal"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sid, Path: "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 只有通过 HTTPS 访问时才打 Secure，否则本地 http 开发登不上
		Secure: strings.HasPrefix(s.cfg.BaseURL, "https://"),
	})
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/admin" // 只允许站内相对跳转，挡住 open redirect
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.db.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
