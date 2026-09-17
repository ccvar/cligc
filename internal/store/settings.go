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
	// SiteTitle / SiteDescription 是站点自己的名字和一句话描述。
	//
	// 放在这里而不是只当命令行参数：这两个是站长随时会改的内容，
	// 而且直接决定 <title> 和 meta description——改一次要重启服务不合理。
	// 留空则退回命令行的 -title / -desc。
	SiteTitle       string
	SiteDescription string

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
	keySiteTitle    = "site_title"
	keySiteDesc     = "site_desc"
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
	s := SiteSettings{CommentsEnabled: true} // 没设过就是开着
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
		case keySiteTitle:
			s.SiteTitle = v
		case keySiteDesc:
			s.SiteDescription = v
		}
	}
	return s
}

// SaveSettings 整体写入并刷新缓存。
func (d *DB) SaveSettings(ctx context.Context, in SiteSettings) error {
	in.GoogleVerify = strings.TrimSpace(in.GoogleVerify)
	in.BingVerify = strings.TrimSpace(in.BingVerify)
	in.GA4ID = strings.TrimSpace(in.GA4ID)
	in.IndexNowKey = strings.ToLower(strings.TrimSpace(in.IndexNowKey))
	in.SiteTitle = strings.TrimSpace(in.SiteTitle)
	in.SiteDescription = strings.TrimSpace(in.SiteDescription)

	err := d.tx(ctx, func(t *sql.Tx) error {
		now := time.Now().Unix()
		for _, kv := range []struct{ k, v string }{
			{keyGoogleVerify, in.GoogleVerify},
			{keyBingVerify, in.BingVerify},
			{keyGA4, in.GA4ID},
			{keyIndexNow, in.IndexNowKey},
			{keyCommentsOff, boolOff(in.CommentsEnabled)},
			{keySiteTitle, in.SiteTitle},
			{keySiteDesc, in.SiteDescription},
		} {
			if _, err := t.ExecContext(ctx,
				`insert into settings(key,value,updated_at) values(?,?,?)
				 on conflict(key) do update set value=excluded.value, updated_at=excluded.updated_at`,
				kv.k, kv.v, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	d.settings.Store(&in)
	return nil
}

// boolOff 把"开着"编码成空串。见 SiteSettings.CommentsEnabled 的注释。
func boolOff(enabled bool) string {
	if enabled {
		return ""
	}
	return "1"
}
