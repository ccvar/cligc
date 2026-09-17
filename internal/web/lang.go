package web

import (
	"context"
	"net/http"
	"strings"

	"cligc.com/internal/i18n"
)

type langKey struct{}

// LangFrom 取当前请求的界面语言，没有则返回默认语言。
func LangFrom(ctx context.Context) i18n.Lang {
	if l, ok := ctx.Value(langKey{}).(i18n.Lang); ok {
		return l
	}
	return i18n.Default()
}

// WithLang 识别并剥掉 URL 里的语言前缀。
//
// /en/p/x 会被改写成 /p/x 再交给 mux，于是**所有已有路由一条都不用改**，
// 也不会出现"每条路由注册两遍"那种必然漏掉一两条的写法。
//
// 刻意不按 Accept-Language 自动跳转：Googlebot 基本只用一种 locale 抓取，
// 自动跳转会让它永远只看得到一个版本，另一个语种等于不存在。浏览器语言
// 只用来决定要不要显示一个**可关闭的**切换提示，而那是模板层的事。
func (s *Server) WithLang(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := i18n.Default()
		p := r.URL.Path

		if seg, rest, ok := splitFirstSegment(p); ok && seg != "" {
			if l, found := i18n.ByPrefix(seg); found && l.Prefix != "" {
				// 站长没启用的语言，**公开页**直接 404。
				//
				// 光把它从切换器和 hreflang 里去掉不够：那些路径仍然能被
				// 直接访问，也就仍然能被爬到——而它们是内容为空的列表页。
				// 一个站上出现十个空列表页，正是搜索引擎判定"低质量"的典型
				// 形态，而且它们还会分掉抓取预算。
				//
				// 但后台不受这条限制：后台的界面语言是站长自己的偏好，和
				// "这个站对外提供哪些语言"完全是两回事。只写中文的站，
				// 站长照样可能想用英文后台。
				target := rest
				if target == "" {
					target = "/"
				}
				if !isAdminPath(target) &&
					!s.db.Settings(r.Context()).LangEnabled(l.Code, i18n.Default().Code) {
					http.NotFound(w, r)
					return
				}
				lang = l
				// /en -> /，/en/p/x -> /p/x
				if rest == "" {
					rest = "/"
				}
				r2 := r.Clone(r.Context())
				r2.URL.Path = rest
				r = r2
				p = rest
			}
		}

		// 默认语言不该有前缀。/zh/... 重定向到 /...，避免同一内容两个 URL。
		if seg, rest, ok := splitFirstSegment(p); ok && seg == "zh" {
			if rest == "" {
				rest = "/"
			}
			http.Redirect(w, r, rest, http.StatusMovedPermanently)
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), langKey{}, lang)))
	})
}

// splitFirstSegment 把 /a/b/c 拆成 ("a", "/b/c", true)。
func splitFirstSegment(p string) (string, string, bool) {
	if !strings.HasPrefix(p, "/") {
		return "", p, false
	}
	rest := p[1:]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return rest, "", true
	}
	return rest[:i], rest[i:], true
}

// langPath 给站内路径加上语言前缀。默认语言不加，其余加 /<prefix>。
func langPath(l i18n.Lang, path string) string {
	if l.Prefix == "" || path == "" || !strings.HasPrefix(path, "/") {
		return path
	}
	if path == "/" {
		return "/" + l.Prefix + "/"
	}
	return "/" + l.Prefix + path
}

// tr 按当前请求的语言取一条文案，args 依次替换 {0} {1}…
//
// Go 这边的用户可见文字（flash、错误页）和模板里的文案共用同一套目录：
// 两份目录必然会走偏，而 flash 恰恰是最容易被忘记翻译的地方——它只在
// 操作成功或失败的一瞬间出现，测试和肉眼巡查都容易漏掉。
func (s *Server) tr(r *http.Request, key string, args ...string) string {
	return i18n.F(LangFrom(r.Context()).Code, key, args...)
}

// trn 按当前语言取一条带数量的文案。
func (s *Server) trn(r *http.Request, key string, n int) string {
	return i18n.N(LangFrom(r.Context()).Code, key, n)
}

// isAdminPath 报告这个路径是不是后台页面（语言前缀已经剥掉）。
//
// 单独一个函数而不是内联判断：render() 里也要用同一套规则决定加载哪层
// 样式，两处各写一遍迟早对不上。
func isAdminPath(p string) bool {
	return strings.HasPrefix(p, "/admin") || strings.HasPrefix(p, "/login")
}
