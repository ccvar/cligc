// cligc 是一个单二进制的轻量内容站：服务端渲染的网页、一套 REST API、
// 以及一个把这套 API 暴露给 AI 客户端的 MCP 服务端。
//
// 子命令：
//
//	cligc serve          启动站点（网页 + API + /mcp 端点）
//	cligc mcp            以 stdio 方式跑 MCP 服务端，供本地 AI 客户端拉起
//	cligc user add       创建用户
//	cligc token create   创建 API token
//	cligc rerender       渲染器变更后重刷全部文章的 HTML 缓存
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cligc.com/internal/api"
	"cligc.com/internal/i18n"
	"cligc.com/internal/indexnow"
	"cligc.com/internal/mcp"
	"cligc.com/internal/store"
	"cligc.com/internal/web"
)

// version 由 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "mcp":
		err = cmdMCP(os.Args[2:])
	case "user":
		err = cmdUser(os.Args[2:])
	case "token":
		err = cmdToken(os.Args[2:])
	case "rerender":
		err = cmdRerender(os.Args[2:])
	case "lang":
		err = cmdLang(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	case "version", "--version":
		fmt.Println("cligc", version)
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cligc — 轻量内容站（Go + SQLite，单二进制）

用法:
  cligc serve  [flags]          启动站点
  cligc mcp    [flags]          以 stdio 跑 MCP 服务端（给本地 AI 客户端）
  cligc user   add  [flags]     创建用户
  cligc token  create [flags]   创建 API token
  cligc rerender [flags]        重刷全部文章的 HTML 缓存与全文索引
  cligc lang   new <code>       生成一份新语言的文案模板（JSON）
  cligc lang   check [dir]      检查语言目录的覆盖率和多余 key

各子命令加 -h 查看参数。
`)
}

// env 读环境变量，缺省时用 def。
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// openDB 打开数据库，并确保目录存在。
func openDB(path, host string) (*store.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return store.Open(path, host)
}

// hostOf 从 base URL 里取主机名，供渲染器识别站外链接。
func hostOf(baseURL string) string {
	s := baseURL
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	return s
}

// --- serve ---

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", env("CLIGC_DB", "data/cligc.db"), "SQLite 数据库路径")
	addr := fs.String("addr", env("CLIGC_ADDR", ":8866"), "监听地址")
	baseURL := fs.String("base-url", env("CLIGC_BASE_URL", "http://localhost:8866"), "站点对外地址")
	title := fs.String("title", env("CLIGC_TITLE", "cligc"), "站点标题")
	desc := fs.String("desc", env("CLIGC_DESC", ""), "站点描述")
	mediaDir := fs.String("media", env("CLIGC_MEDIA", "data/media"), "上传文件目录")
	dailyCap := fs.Int("daily-cap", envInt("CLIGC_DAILY_CAP", 5),
		"每用户每日发布上限，0 表示不限")
	trustProxy := fs.Bool("trust-proxy", os.Getenv("CLIGC_TRUST_PROXY") != "",
		"相信 X-Forwarded-For。只在确实部署在自己的反向代理后面时打开")
	comments := fs.Bool("comments", os.Getenv("CLIGC_COMMENTS") != "0",
		"开放评论。匿名评论一律进人工审核队列")
	perPage := fs.Int("per-page", envInt("CLIGC_PER_PAGE", 20), "每页条数")
	queueCap := fs.Int("comment-queue-cap", envInt("CLIGC_COMMENT_QUEUE_CAP", 500),
		"待审评论队列上限，满了暂停接收匿名评论；0 表示不限")
	langDir := fs.String("lang-dir", env("CLIGC_LANG_DIR", ""),
		"额外的界面语言目录（*.json）。放进去就能加语言，不用重新编译")
	fs.Parse(args)

	if err := os.MkdirAll(*mediaDir, 0o755); err != nil {
		return err
	}

	// 先加载内置目录（它决定默认语言和覆盖率基准），再叠加磁盘上的。
	// 磁盘那份可以只写想改的几条，其余沿用内置。
	if err := i18n.LoadBuiltin(); err != nil {
		return fmt.Errorf("载入内置语言目录: %w", err)
	}
	if n, err := i18n.LoadDir(*langDir); err != nil {
		return fmt.Errorf("载入 %s: %w", *langDir, err)
	} else if n > 0 {
		fmt.Printf("从 %s 载入了 %d 个语言目录\n", *langDir, n)
	}
	for _, l := range i18n.Languages() {
		if !l.Ready() {
			fmt.Fprintf(os.Stderr,
				"提示：%s（%s）翻译覆盖率 %.0f%%，低于 %.0f%% 不会出现在语言切换器里\n",
				l.Name, l.Code, l.Coverage*100, i18n.MinCoverage*100)
		}
	}
	db, err := openDB(*dbPath, hostOf(*baseURL))
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if n, err := db.CountUsers(ctx); err == nil && n == 0 {
		fmt.Fprintln(os.Stderr,
			"提示：还没有任何用户，先运行  cligc user add -email you@example.com -name 你的名字 -admin")
	}

	// IndexNow。key 来自后台设置页，可以随时改，所以这里只给 baseURL；
	// 没设 key 时 Ping 是空操作。
	pinger := indexnow.New(*baseURL)
	pinger.SetKey(db.Settings(ctx).IndexNowKey)
	db.OnPostChanged = func(p *store.Post) {
		l, _ := i18n.ByCode(p.Lang)
		prefix := ""
		if l.Prefix != "" {
			prefix = "/" + l.Prefix
		}
		pinger.Ping(prefix + "/p/" + p.Slug)
	}

	webSrv, err := web.New(db, web.Config{
		BaseURL: *baseURL, Title: *title, Description: *desc,
		MediaRoot: *mediaDir, DailyPublishCap: *dailyCap, TrustProxy: *trustProxy,
		CommentsEnabled: *comments, PerPage: *perPage, CommentQueueCap: *queueCap,
		IndexNow: pinger,
	})
	if err != nil {
		return err
	}
	apiSrv := api.New(db, api.Config{
		BaseURL: *baseURL, MediaRoot: *mediaDir, DailyPublishCap: *dailyCap,
		// 通过 API 改了设置之后要把新 key 同步给提交器。只写库不同步的话，
		// 下一次发布还在用旧 key，而且不报错——表现是"提交了但一直 403"。
		OnSettingsSaved: func(st store.SiteSettings) { pinger.SetKey(st.IndexNowKey) },
	})
	// MCP 的 HTTP 传输挂在同一个进程里，通过回环调自己的 API。
	// 这样 MCP 只有一条代码路径，不需要为"内嵌"和"远程"写两套。
	mcpSrv := &mcp.Server{
		BaseURL: "http://127.0.0.1" + localPort(*addr),
		Name:    "cligc", Version: version,
	}

	mux := http.NewServeMux()
	webSrv.Routes(mux)
	apiSrv.Routes(mux)
	mux.Handle("/mcp", mcpSrv.HTTPHandler())

	// WithLang 必须在最外层：它要在 mux 匹配之前把 /en/ 前缀剥掉，
	// 否则每条路由都得注册两遍。
	// IndexNow 校验文件。路径固定、内容动态：key 是后台里随时能改的设置，
	// 而路由在启动时就注册完了。提交时用 keyLocation 指明这个位置。
	mux.HandleFunc("GET "+indexnow.KeyPath, func(w http.ResponseWriter, r *http.Request) {
		key := pinger.Key()
		if key == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, key)
	})

	// CSP 每次请求现算：GA4 是后台里的开关，不能在启动时定死。
	handler := logging(secureHeaders(func() bool {
		return db.Settings(context.Background()).GA4ID != ""
	}, webSrv.WithLang(webSrv.WithSession(mux))))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 后台定期清理过期会话和过期幂等键
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := db.Vacuum(ctx); err != nil {
					fmt.Fprintln(os.Stderr, "vacuum:", err)
				}
			}
		}
	}()

	// 定时发布。每分钟扫一次到点的草稿——分钟级精度对内容站足够，
	// 而更密的轮询只是在空转。
	//
	// 起来先跑一次：进程重启期间到点的那些不能一直压着。停机一小时再开，
	// 期间该发的会在启动时立刻补发，而不是等下一个整分钟。
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		run := func() {
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			n, errs := db.PublishDue(c, time.Now())
			if n > 0 {
				log.Printf("定时发布 %d 篇", n)
			}
			for _, err := range errs {
				log.Printf("定时发布失败: %v", err)
			}
		}
		run()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				run()
			}
		}
	}()

	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()

	fmt.Printf("cligc %s 监听 %s（对外地址 %s）\n", version, *addr, *baseURL)
	if *dailyCap > 0 {
		fmt.Printf("每日发布上限：%d 篇/人\n", *dailyCap)
	}
	if st := db.Settings(ctx); st.IndexNowKey != "" || st.GA4ID != "" {
		if st.IndexNowKey != "" {
			fmt.Printf("IndexNow 已启用，校验文件 %s\n", pinger.KeyFileURL())
		}
		if st.GA4ID != "" {
			fmt.Printf("GA4 已启用（%s）：script-src 已放开到 googletagmanager.com\n", st.GA4ID)
		}
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	fmt.Println("已关闭")
	return nil
}

// localPort 从监听地址里取出端口，拼成回环地址用。
func localPort(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":8866"
}

// secureHeaders 给所有响应加安全响应头。
//
// CSP 是纵深防御：正文已经在 goldmark 里转义过原始 HTML，即便某天那层
// 出了问题，脚本也执行不起来。script-src 不影响文章页里的
// application/ld+json——那是数据块，不会被当作脚本执行。
func secureHeaders(ga4Enabled func() bool, next http.Handler) http.Handler {
	// 两份 CSP 预先算好，请求时只做一次布尔判断。GA4 是后台里的开关，
	// 不能在启动时定死——但也没必要为此每个请求都重新拼字符串。
	build := func(ga4 bool) string {
		script := "'self'"
		connect := "'self'"
		if ga4 {
			// 只放开它实际需要的那两个域名。没开 GA4 时那条
			// script-src 'self' 一个字不动——为一个没启用的功能常年把
			// CSP 敞着，是最容易被忽略的那种退化。
			script += " https://www.googletagmanager.com"
			connect += " https://www.google-analytics.com https://*.analytics.google.com"
		}
		return "default-src 'self'; img-src 'self' data: https:; " +
			"style-src 'self'; script-src " + script + "; connect-src " + connect + "; " +
			"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	}
	strict, loose := build(false), build(true)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		csp := strict
		if ga4Enabled() {
			csp = loose
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// statusWriter 记录实际写出的状态码，供访问日志使用。
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		fmt.Printf("%s %s %d %s\n", r.Method, r.URL.Path, sw.code,
			time.Since(start).Round(time.Millisecond))
	})
}

// --- mcp ---

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	baseURL := fs.String("base-url", env("CLIGC_BASE_URL", "http://localhost:8866"),
		"站点地址")
	token := fs.String("token", env("CLIGC_TOKEN", ""),
		"API token（建议用环境变量 CLIGC_TOKEN 传，避免出现在进程列表里）")
	fs.Parse(args)

	// stdout 是 MCP 的传输通道，任何多余输出都会破坏协议。
	// 所有诊断信息必须走 stderr。
	if *token == "" {
		fmt.Fprintln(os.Stderr, "警告：未设置 CLIGC_TOKEN，工具调用会返回未配置提示")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &mcp.Server{BaseURL: *baseURL, Name: "cligc", Version: version}
	return srv.ServeStdio(ctx, *token, os.Stdin, os.Stdout)
}

// --- user ---

func cmdUser(args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("用法: cligc user add -email <邮箱> -name <名字> [-admin]")
	}
	fs := flag.NewFlagSet("user add", flag.ExitOnError)
	dbPath := fs.String("db", env("CLIGC_DB", "data/cligc.db"), "SQLite 数据库路径")
	email := fs.String("email", "", "邮箱（登录用）")
	name := fs.String("name", "", "显示名")
	password := fs.String("password", "", "密码，留空则自动生成并打印")
	admin := fs.Bool("admin", false, "设为管理员")
	fs.Parse(args[1:])

	if *email == "" || *name == "" {
		return errors.New("-email 和 -name 必填")
	}
	pw := *password
	generated := false
	if pw == "" {
		pw = store.RandomSlug() + store.RandomSlug()
		generated = true
	}
	db, err := openDB(*dbPath, "")
	if err != nil {
		return err
	}
	defer db.Close()

	role := "author"
	if *admin {
		role = "admin"
	}
	u, err := db.CreateUser(context.Background(), *email, *name, pw, role)
	if err != nil {
		return err
	}
	fmt.Printf("已创建用户 #%d  %s  <%s>  角色=%s\n", u.ID, u.Name, u.Email, u.Role)
	if generated {
		fmt.Printf("密码（只显示这一次）：%s\n", pw)
	}
	return nil
}

// --- token ---

func cmdToken(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("用法: cligc token create -email <邮箱> -name <名称> [-scopes ...] [-days N]")
	}
	fs := flag.NewFlagSet("token create", flag.ExitOnError)
	dbPath := fs.String("db", env("CLIGC_DB", "data/cligc.db"), "SQLite 数据库路径")
	email := fs.String("email", "", "token 所属用户的邮箱")
	name := fs.String("name", "", "token 名称，如 claude-code-laptop")
	scopes := fs.String("scopes", "posts:read,posts:write,media:write",
		"逗号分隔。默认不含 posts:publish —— AI 写草稿，人来发布")
	days := fs.Int("days", 90, "有效天数，0 表示永久")
	fs.Parse(args[1:])

	if *email == "" || *name == "" {
		return errors.New("-email 和 -name 必填")
	}
	db, err := openDB(*dbPath, "")
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	var uid int64
	if err := db.R.QueryRowContext(ctx, `select id from users where email=?`,
		strings.ToLower(strings.TrimSpace(*email))).Scan(&uid); err != nil {
		return fmt.Errorf("找不到用户 %s：%w", *email, err)
	}
	var ttl time.Duration
	if *days > 0 {
		ttl = time.Duration(*days) * 24 * time.Hour
	}
	plain, tok, err := db.CreateToken(ctx, uid, *name, strings.Split(*scopes, ","), ttl)
	if err != nil {
		return err
	}
	fmt.Printf("token 名称: %s\n权限: %s\n", tok.Name, strings.Join(tok.Scopes, " "))
	fmt.Printf("\n%s\n\n", plain)
	fmt.Println("只显示这一次，库里只存它的 SHA-256。")
	return nil
}

// --- rerender ---

// cmdRerender 在渲染器发生变化后重刷全部文章。
//
// body_html 和全文索引都是 body_md 的派生物，加代码高亮、改外链规则这类
// 改动只影响新写的文章；老文章要靠这条命令补上。
func cmdRerender(args []string) error {
	fs := flag.NewFlagSet("rerender", flag.ExitOnError)
	dbPath := fs.String("db", env("CLIGC_DB", "data/cligc.db"), "SQLite 数据库路径")
	baseURL := fs.String("base-url", env("CLIGC_BASE_URL", "http://localhost:8866"),
		"站点地址，用于识别站外链接")
	fs.Parse(args)

	db, err := openDB(*dbPath, hostOf(*baseURL))
	if err != nil {
		return err
	}
	defer db.Close()

	n, err := db.RerenderAll(context.Background())
	if err != nil {
		return fmt.Errorf("重刷到第 %d 篇时失败: %w", n, err)
	}
	fmt.Printf("已重刷 %d 篇文章的 HTML 缓存与全文索引\n", n)
	return nil
}

// --- lang ---

// cmdLang 管理界面语言目录。
//
// 加一种界面语言不需要改代码、不需要重新编译：
//
//	cligc lang new ja > locales/ja.json   # 生成模板（值是中文原文，照着翻）
//	cligc serve -lang-dir locales          # 启动时叠加上去
//
// 这条路径是刻意留的——一万条文案不可能写在 Go 源码里，也不该要求
// 每加一种语言就发一个新版本。
func cmdLang(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: cligc lang new <code> | cligc lang check [dir]")
	}
	if err := i18n.LoadBuiltin(); err != nil {
		return err
	}
	switch args[0] {
	case "new":
		if len(args) < 2 {
			return errors.New("用法: cligc lang new <code>，如 cligc lang new ja")
		}
		return langNew(args[1])
	case "check":
		dir := ""
		if len(args) > 1 {
			dir = args[1]
		}
		return langCheck(dir)
	default:
		return fmt.Errorf("未知子命令 %q", args[0])
	}
}

func langNew(code string) error {
	if !i18n.Known(code) {
		fmt.Fprintf(os.Stderr,
			"提示：%q 不在语言注册表里，母语名会直接用这个标签。\n"+
				"要让切换器显示正确的母语名，把它加进 internal/i18n/langs.go。\n", code)
	}
	// 值填成默认语言的原文而不是空串：翻译的人看得到要翻什么，
	// 而且没翻完的条目会原样显示成中文，比空白更容易被发现。
	msgs := map[string]any{}
	for _, k := range i18n.Keys() {
		msgs[k] = i18n.T(i18n.DefaultCode, k)
	}
	doc := map[string]any{
		"lang": code,
		"name": i18n.Name(code),
		"_comment": "把 messages 里的值翻成目标语言。值和原文完全相同的条目算作未翻译，" +
			"所以覆盖率会如实反映进度。带复数的条目写成 {\"one\":…,\"other\":…}，具体需要哪几种形式由 CLDR 决定。",
		"messages": msgs,
	}
	b, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func langCheck(dir string) error {
	if dir != "" {
		n, err := i18n.LoadDir(dir)
		if err != nil {
			return err
		}
		fmt.Printf("从 %s 载入 %d 个目录\n\n", dir, n)
	}
	fmt.Printf("%-10s %-16s %7s %7s %9s  %s\n", "标签", "名称", "覆盖率", "缺失", "同原文", "状态")
	for _, l := range i18n.Languages() {
		miss, same := i18n.Missing(l.Code), i18n.Untranslated(l.Code)
		status := "OK"
		if !l.Ready() {
			status = fmt.Sprintf("低于 %.0f%%，不上架", i18n.MinCoverage*100)
		}
		fmt.Printf("%-10s %-16s %6.1f%% %7d %9d  %s\n",
			l.Code, l.Name, l.Coverage*100, len(miss), len(same), status)
		// 缺失是硬缺陷，值得逐条列出来；同原文的可能是合法的，只报个数
		for i, k := range miss {
			if i >= 5 {
				fmt.Printf("           …还有 %d 条缺失\n", len(miss)-5)
				break
			}
			fmt.Printf("           缺失: %s\n", k)
		}
	}
	return nil
}
