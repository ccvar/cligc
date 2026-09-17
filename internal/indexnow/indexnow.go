// Package indexnow 把"这个 URL 变了"这件事告诉支持 IndexNow 的搜索引擎。
//
// 协议本身极简：在站点上放一个内容等于 key 的文本文件，然后每次内容变动时
// 请求一次 api.indexnow.org。Bing、Yandex、Seznam、Naver 共享同一个端点，
// 提交一次即可。
//
// Google 不在其中——它公开表示过不使用 IndexNow，那边只能靠 sitemap 和
// 正常抓取。所以这个包解决的是"Bing 系多久能看到新内容"，不是全部。
//
// 为什么值得做：这个站的整个论点就是能不能被收录。而 IndexNow 是纯服务端
// 的一次 HTTP 请求——不需要 JS、不动 CSP、不引第三方脚本，跟这套架构完全
// 不冲突。代价只有一个出站请求。
package indexnow

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const endpoint = "https://api.indexnow.org/indexnow"

// KeyPath 是校验文件挂载的固定路径。
//
// 协议默认要求文件叫 <key>.txt 放在根目录，但也允许放在任意位置、提交时用
// keyLocation 指明。这里选后者：key 现在是后台里随时能改的设置，而路由在
// 启动时就注册完了——固定路径 + keyLocation 才能让改 key 不需要重启。
const KeyPath = "/indexnow-key.txt"

// Pinger 按站点持有主机名和一个可以随时替换的 key。
type Pinger struct {
	baseURL string
	key     atomic.Pointer[string]
	client  *http.Client
	ch      chan string
}

// New 返回一个 Pinger。baseURL 无效时返回 nil——调用方对 nil 调用任何方法
// 都是安全的空操作，接入点因此不用到处判空。
func New(baseURL string) *Pinger {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil
	}
	p := &Pinger{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 10 * time.Second},
		// 带缓冲：提交是尽力而为的通知，队列满了就丢，绝不能反压到发布流程上。
		ch: make(chan string, 256),
	}
	go p.loop()
	return p
}

// SetKey 换上新的 key。空串表示关闭提交。
func (p *Pinger) SetKey(k string) {
	if p == nil {
		return
	}
	k = strings.ToLower(strings.TrimSpace(k))
	p.key.Store(&k)
}

// Key 返回当前 key，没配置时是空串。
func (p *Pinger) Key() string {
	if p == nil {
		return ""
	}
	if k := p.key.Load(); k != nil {
		return *k
	}
	return ""
}

// Enabled 报告当前是否会真的往外提交。
func (p *Pinger) Enabled() bool { return p.Key() != "" }

// KeyFileURL 返回校验文件的绝对地址，用于提交时的 keyLocation。
func (p *Pinger) KeyFileURL() string {
	if p == nil {
		return ""
	}
	return p.baseURL + KeyPath
}

// Ping 通知搜索引擎某个站内路径变了。path 是站内绝对路径，如 "/p/foo"。
//
// 非阻塞：发布一篇文章不该因为第三方端点慢或者挂了就卡住，更不该失败。
// 队列满时直接丢弃——漏掉一次提交只是晚一点被抓到，而阻塞发布是真故障。
func (p *Pinger) Ping(path string) {
	if p == nil || path == "" || !p.Enabled() {
		return
	}
	select {
	case p.ch <- p.baseURL + path:
	default:
	}
}

func (p *Pinger) loop() {
	for u := range p.ch {
		p.submit(u)
	}
}

func (p *Pinger) submit(u string) {
	key := p.Key()
	if key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q := url.Values{"url": {u}, "key": {key}, "keyLocation": {p.KeyFileURL()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return
	}
	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("indexnow: %s: %v", u, err)
		return
	}
	defer resp.Body.Close()
	// 200 和 202 都算收下了。其余状态码值得记一笔——配置错误不报出来就
	// 永远不会被发现，表现只是"提交了但一直没人来抓"。
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted:
	case http.StatusForbidden:
		// 这一种几乎总是同一个原因，直接把排查方向写出来
		log.Printf("indexnow: %s: 403 —— 校验文件 %s 打不开，或者内容和 key 不一致",
			u, p.KeyFileURL())
	default:
		log.Printf("indexnow: %s: %s", u, resp.Status)
	}
}
