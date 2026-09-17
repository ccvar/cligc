package web

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"cligc.com/internal/i18n"
	"cligc.com/internal/store"
)

// sitemapChunk 是单个 sitemap 文件里的最大 URL 数。协议上限是 50000，
// 取小一些让每个文件更快生成、更快被抓完。
const sitemapChunk = 5000

type urlEntry struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type urlSet struct {
	XMLName xml.Name   `xml:"urlset"`
	NS      string     `xml:"xmlns,attr"`
	URLs    []urlEntry `xml:"url"`
}

type sitemapRef struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type sitemapIndex struct {
	XMLName  xml.Name     `xml:"sitemapindex"`
	NS       string       `xml:"xmlns,attr"`
	Sitemaps []sitemapRef `xml:"sitemap"`
}

const sitemapNS = "http://www.sitemaps.org/schemas/sitemap/0.9"

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	fmt.Fprint(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	enc.Encode(v)
}

// handleSitemap 输出 sitemap。
//
// 只收录"值得收录"的页面：已发布、允许索引、且没有指向站外的 canonical。
// 标签页、作者页、搜索页、分页一律不进 sitemap —— 把爬虫的抓取预算
// 集中到文章本体上，这是新站能不能被收录的关键变量之一。
//
// 超过一个分片时自动降级成 sitemap index，不需要改配置。
func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	total, err := s.db.CountIndexable(r.Context())
	if err != nil {
		http.Error(w, "sitemap unavailable", http.StatusInternalServerError)
		return
	}
	if total > sitemapChunk {
		n := (total + sitemapChunk - 1) / sitemapChunk
		idx := sitemapIndex{NS: sitemapNS}
		for i := 1; i <= n; i++ {
			idx.Sitemaps = append(idx.Sitemaps, sitemapRef{
				Loc: fmt.Sprintf("%s/sitemaps/%d", s.cfg.BaseURL, i),
			})
		}
		writeXML(w, idx)
		return
	}
	s.writeSitemapChunk(w, r, 1, true)
}

// handleSitemapChunk 输出第 n 个分片。
func (s *Server) handleSitemapChunk(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	s.writeSitemapChunk(w, r, n, false)
}

func (s *Server) writeSitemapChunk(w http.ResponseWriter, r *http.Request, n int, withHome bool) {
	entries, err := s.db.IndexableEntries(r.Context(), sitemapChunk, (n-1)*sitemapChunk)
	if err != nil {
		http.Error(w, "sitemap unavailable", http.StatusInternalServerError)
		return
	}
	if len(entries) == 0 && !withHome {
		http.NotFound(w, r)
		return
	}
	set := urlSet{NS: sitemapNS}
	if withHome {
		// 每个语言的首页都要收录
		for _, l := range i18n.ReadyLanguages() {
			set.URLs = append(set.URLs, urlEntry{Loc: s.cfg.BaseURL + langPath(l, "/")})
		}
	}
	for _, e := range entries {
		// URL 要带上该文章自己语言的前缀。sitemap 是站级的、不分语言，
		// 但每条 URL 必须是那一版真实可访问的地址。
		l, _ := i18n.ByCode(e.Lang)
		set.URLs = append(set.URLs, urlEntry{
			Loc:     s.cfg.BaseURL + langPath(l, "/p/"+e.Slug),
			LastMod: e.UpdatedAt.Format("2006-01-02"),
		})
	}
	writeXML(w, set)
}

// handleRobots 输出 robots.txt。
//
// 屏蔽掉后台、登录和站内搜索：搜索结果页由用户输入生成，数量无限，
// 放任爬虫抓取会大量消耗抓取预算，且这类页面本身就是搜索引擎明确
// 点名的低质量类型。标签页不屏蔽——它们靠页面里的 noindex 控制，
// 仍然需要被抓取以传递内链权重。
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	fmt.Fprintf(w, `User-agent: *
Allow: /
Disallow: /admin
Disallow: /login
Disallow: /search
Disallow: /api/

Sitemap: %s/sitemap.xml
`, s.cfg.BaseURL)
}

// --- RSS ---

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
	Author      string `xml:"dc:creator,omitempty"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	Items       []rssItem `xml:"item"`
}

type rss struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	DC      string     `xml:"xmlns:dc,attr"`
	Channel rssChannel `xml:"channel"`
}

// handleFeed 输出 RSS。只放摘要不放全文：全文输出会让内容农场
// 一键镜像整站，而重复内容的归并结果通常不利于原站。
func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	lang := LangFrom(r.Context())
	posts, _, err := s.db.ListPosts(r.Context(), store.ListFilter{
		Status: store.StatusPublished, Lang: lang.Code, Limit: 20,
	})
	if err != nil {
		http.Error(w, "feed unavailable", http.StatusInternalServerError)
		return
	}
	ch := rssChannel{
		Title: s.cfg.Title, Link: s.cfg.BaseURL + langPath(lang, "/"),
		Description: s.cfg.Description, Language: lang.Code,
	}
	for _, p := range posts {
		pub := p.CreatedAt
		if p.PublishedAt != nil {
			pub = *p.PublishedAt
		}
		link := s.cfg.BaseURL + langPath(lang, "/p/"+p.Slug)
		ch.Items = append(ch.Items, rssItem{
			Title: p.Title, Link: link, GUID: link,
			PubDate:     pub.Format(time.RFC1123Z),
			Description: p.Summary,
			Author:      p.AuthorName,
		})
	}
	writeXML(w, rss{Version: "2.0", DC: "http://purl.org/dc/elements/1.1/", Channel: ch})
}
