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
}

const (
	keyGoogleVerify = "google_verify"
	keyBingVerify   = "bing_verify"
	keyGA4          = "ga4_id"
	keyIndexNow     = "indexnow_key"
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
	var s SiteSettings
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

	err := d.tx(ctx, func(t *sql.Tx) error {
		now := time.Now().Unix()
		for _, kv := range []struct{ k, v string }{
			{keyGoogleVerify, in.GoogleVerify},
			{keyBingVerify, in.BingVerify},
			{keyGA4, in.GA4ID},
			{keyIndexNow, in.IndexNowKey},
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
