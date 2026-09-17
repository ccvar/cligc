package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// SiteSettings 是站长在后台填的那些值。
//
// 全部是从第三方网页控制台复制过来的凭据或 ID——拿到手就粘贴一次，之后
// 基本不动。放在库里而不是命令行参数上，是因为"改一个统计 ID 要重启服务"
// 不是一个合理的流程。
type SiteSettings struct {
	// SiteTitles / SiteDescs 是**按语言**的站名和描述，key 是语言代码。
	//
	// 按语言存而不是只存一份：站点开了英文版，<title> 和 meta description
	// 却还是中文的话，那一版在英文搜索结果里根本读不通。
	// 取值时先找当前语言，找不到退回默认语言，再找不到退回命令行参数。
	SiteTitles map[string]string
	SiteDescs  map[string]string

	// EnabledLangs 是站点实际对外提供的语言，空表示只有默认语言。
	//
	// 这不只是界面开关。没有它的话，站上明明只写中文，却会给 11 种语言
	// 都发 hreflang——而那些页面是空的。等于主动告诉搜索引擎"这里有德语版"，
	// 然后给它一个没有任何文章的列表页。
	EnabledLangs []string

	// GoogleVerify 是 Search Console 的 google-site-verification 值。
	GoogleVerify string
	// BingVerify 是 Bing 站长平台的 msvalidate.01 值。
	BingVerify string
	// GA4ID 是 Google Analytics 4 的衡量 ID（G-XXXXXXX）。
	// 它是这四个里唯一有运行时代价的：设了就要加载第三方脚本，
	// 并把 CSP 的 script-src 放开到 googletagmanager.com。
	GA4ID string
	// IndexNowKey 设了就在发布后主动通知 Bing/Yandex/Seznam/Naver。
	// Google 不支持 IndexNow，那边仍然只能靠 sitemap 和正常抓取。
	IndexNowKey string

	// CommentsEnabled 控制是否开放评论。默认开。
	//
	// 库里存的是反过来的 comments_off："这一行不存在"要等于"开着"，
	// 否则从没进过设置页的站会因为读到空值而把评论关掉。
	CommentsEnabled bool
}

const (
	keyGoogleVerify = "google_verify"
	keyBingVerify   = "bing_verify"
	keyGA4          = "ga4_id"
	keyIndexNow     = "indexnow_key"
	keyCommentsOff  = "comments_off"
	keyEnabledLangs = "enabled_langs"
	// 按语言的站名/描述用前缀 + 语言代码，如 site_title:en
	prefixSiteTitle = "site_title:"
	prefixSiteDesc  = "site_desc:"
)

// Settings 返回当前设置。读的是内存快照——它在每个页面的 <head> 里都要用到，
// 每次渲染都回库查一遍没有意义。
func (d *DB) Settings(ctx context.Context) SiteSettings {
	if s := d.settings.Load(); s != nil {
		return *s
	}
	s := d.loadSettings(ctx)
	d.settings.Store(&s)
	return s
}

func (d *DB) loadSettings(ctx context.Context) SiteSettings {
	s := SiteSettings{
		CommentsEnabled: true, // 没设过就是开着
		SiteTitles:      map[string]string{},
		SiteDescs:       map[string]string{},
	}
	rows, err := d.R.QueryContext(ctx, `select key, value from settings`)
	if err != nil {
		return s
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return s
		}
		switch k {
		case keyGoogleVerify:
			s.GoogleVerify = v
		case keyBingVerify:
			s.BingVerify = v
		case keyGA4:
			s.GA4ID = v
		case keyIndexNow:
			s.IndexNowKey = v
		case keyCommentsOff:
			s.CommentsEnabled = v == ""
		case keyEnabledLangs:
			if v != "" {
				s.EnabledLangs = strings.Split(v, ",")
			}
		default:
			if code, ok := strings.CutPrefix(k, prefixSiteTitle); ok {
				s.SiteTitles[code] = v
			} else if code, ok := strings.CutPrefix(k, prefixSiteDesc); ok {
				s.SiteDescs[code] = v
			}
		}
	}
	return s
}

// SaveSettings 整体写入并刷新缓存。
//
// 按语言的站名/描述是动态 key，条数随语言数变，所以不能像固定项那样
// 逐个 upsert 完事——取消勾选一个语言之后，它那两行必须真的消失，
// 否则重新启用时会冒出一份没人记得设过的旧文案。
func (d *DB) SaveSettings(ctx context.Context, in SiteSettings) error {
	in.GoogleVerify = strings.TrimSpace(in.GoogleVerify)
	in.BingVerify = strings.TrimSpace(in.BingVerify)
	in.GA4ID = strings.TrimSpace(in.GA4ID)
	in.IndexNowKey = strings.ToLower(strings.TrimSpace(in.IndexNowKey))

	fixed := []struct{ k, v string }{
		{keyGoogleVerify, in.GoogleVerify},
		{keyBingVerify, in.BingVerify},
		{keyGA4, in.GA4ID},
		{keyIndexNow, in.IndexNowKey},
		{keyCommentsOff, boolOff(in.CommentsEnabled)},
		{keyEnabledLangs, strings.Join(in.EnabledLangs, ",")},
	}

	err := d.tx(ctx, func(t *sql.Tx) error {
		now := time.Now().Unix()
		up := func(k, v string) error {
			_, err := t.ExecContext(ctx,
				`insert into settings(key,value,updated_at) values(?,?,?)
				 on conflict(key) do update set value=excluded.value, updated_at=excluded.updated_at`,
				k, v, now)
			return err
		}
		for _, kv := range fixed {
			if err := up(kv.k, kv.v); err != nil {
				return err
			}
		}
		// 动态 key 先清后写
		if _, err := t.ExecContext(ctx,
			`delete from settings where key like ? or key like ?`,
			prefixSiteTitle+"%", prefixSiteDesc+"%"); err != nil {
			return err
		}
		for code, v := range in.SiteTitles {
			if v = strings.TrimSpace(v); v != "" {
				if err := up(prefixSiteTitle+code, v); err != nil {
					return err
				}
			}
		}
		for code, v := range in.SiteDescs {
			if v = strings.TrimSpace(v); v != "" {
				if err := up(prefixSiteDesc+code, v); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 缓存存的是刚写进去的那份，顺手把空值清掉，免得读回来和库里不一致
	clean := in
	clean.SiteTitles = trimMap(in.SiteTitles)
	clean.SiteDescs = trimMap(in.SiteDescs)
	d.settings.Store(&clean)
	return nil
}

func trimMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if v = strings.TrimSpace(v); v != "" {
			out[k] = v
		}
	}
	return out
}

// SiteTitleFor 按语言取站名，找不到就退回默认语言那一份。
// 两者都没有时返回空串，由调用方退回命令行参数。
func (s SiteSettings) SiteTitleFor(code, defaultCode string) string {
	return pickLang(s.SiteTitles, code, defaultCode)
}

// SiteDescFor 同上，取描述。
func (s SiteSettings) SiteDescFor(code, defaultCode string) string {
	return pickLang(s.SiteDescs, code, defaultCode)
}

func pickLang(m map[string]string, code, defaultCode string) string {
	if v := m[code]; v != "" {
		return v
	}
	return m[defaultCode]
}

// LangEnabled 报告某个语言是否对外提供。空列表表示只有默认语言。
func (s SiteSettings) LangEnabled(code, defaultCode string) bool {
	if code == defaultCode {
		return true // 默认语言永远开着，否则站点没有任何入口
	}
	for _, c := range s.EnabledLangs {
		if c == code {
			return true
		}
	}
	return false
}

// boolOff 把"开着"编码成空串。见 SiteSettings.CommentsEnabled 的注释。
func boolOff(enabled bool) string {
	if enabled {
		return ""
	}
	return "1"
}
