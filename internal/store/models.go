package store

import (
	"errors"
	"time"

	"cligc.com/internal/render"
)

// 领域错误。上层（API / MCP）据此映射 HTTP 状态码，不要用字符串匹配。
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrDailyCap     = errors.New("daily publish cap reached")
	ErrInvalidInput = errors.New("invalid input")
)

// 文章状态。
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusArchived  = "archived"
)

// 内容来源。记录它不是为了给读者看，是为了你自己心里有数：
// 一个站上 ai-generated 的占比一旦失控，搜索引擎那边的风险就是真的。
const (
	SourceHuman       = "human"
	SourceAIAssisted  = "ai-assisted"
	SourceAIGenerated = "ai-generated"
)

// API token 的权限范围。
//
// 站点的每一项功能都有对应的 scope，唯独改密码没有——密码是找回账号的
// 最后一个锚点，一旦能被程序改掉，token 泄露就等于账号彻底丢失。
//
// 分得这么细不是形式主义：每一项单独列出来，站长在勾选框前面才有机会
// 判断"这个客户端到底需不需要它"。合成一个 admin 就没有这个机会了。
const (
	ScopePostsRead    = "posts:read"
	ScopePostsWrite   = "posts:write"
	ScopePostsPublish = "posts:publish"
	ScopeMediaWrite   = "media:write"
	ScopeMediaDelete  = "media:delete"
	// ScopeCommentsModerate 让 token 能通过/退回/删除评论。
	//
	// 这一项值得单独想清楚再勾：评论是站外任何人都能写进来的内容，而
	// AI 会把它们读进上下文。给了这个权限，等于允许"评论正文里写一句
	// 指令 → AI 照做"。这不是假想的攻击，是提示注入最标准的形态。
	ScopeCommentsModerate = "comments:moderate"
	// ScopeTokensManage 让 token 能签发和吊销别的 token。
	// 签发时受"不得超出自身权限"约束，见 CreateTokenAs。
	ScopeTokensManage = "tokens:manage"
	// ScopeSiteAdmin 覆盖站点接入设置和作者资料。
	ScopeSiteAdmin = "site:admin"
)

// AllScopes 是可发放的全部 scope，用于创建 token 时校验。
var AllScopes = []string{
	ScopePostsRead, ScopePostsWrite, ScopePostsPublish,
	ScopeMediaWrite, ScopeMediaDelete,
	ScopeCommentsModerate, ScopeTokensManage, ScopeSiteAdmin,
}

// User 是作者/管理员。
type User struct {
	ID        int64
	Email     string
	Name      string
	Slug      string
	Bio       string
	Role      string // admin | author
	CreatedAt time.Time
}

// IsAdmin 报告该用户是否为管理员。
func (u *User) IsAdmin() bool { return u.Role == "admin" }

// Token 是 API token 的元数据；明文只在创建时返回一次，不入库。
type Token struct {
	ID         int64
	UserID     int64
	Name       string
	Prefix     string
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// Has 报告 token 是否具备某个 scope。
func (t *Token) Has(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Tag 是文章标签。
type Tag struct {
	ID    int64
	Slug  string
	Name  string
	Count int // 仅在带统计的查询里填充
}

// Post 是一篇文章。BodyHTML 是 BodyMD 的渲染缓存，由 store 在写入时维护，
// 调用方不应直接赋值。
type Post struct {
	ID           int64
	UserID       int64
	Slug         string
	Title        string
	Summary      string
	BodyMD       string
	BodyHTML     string
	Status       string
	Source       string
	Indexable    bool
	CanonicalURL string
	Lang         string // BCP 47，决定它出现在哪个语言版本里
	TransKey     string // 同一内容各语言版本共享；空表示这是独立的一篇
	WordCount    int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	PublishedAt  *time.Time
	FeaturedAt   *time.Time // 非空即精选，同时是精选区的排序依据
	// PublishAt 是定时发布的时刻。设了它的文章状态仍然是 draft，到点由
	// 后台任务改成 published——公开列表、sitemap、feed 一律按 status 过滤，
	// 所以"等待发布"的文章不会从任何入口漏出去，不需要额外的过滤条件。
	PublishAt *time.Time

	// 分类：一篇最多一个。CategoryID 为 nil 表示未分类。
	// Slug 和 Name 是查询时 join 出来的，方便模板直接用。
	CategoryID   *int64
	CategorySlug string
	CategoryName string

	// 关联数据，按需填充
	Tags       []Tag
	AuthorName string
	AuthorSlug string
	Headings   []render.Heading // 文章大纲，只在取单篇时填充
}

// IsPublished 报告文章是否处于已发布状态。
func (p *Post) IsPublished() bool { return p.Status == StatusPublished }

// IsFeatured 报告文章是否被置顶到首页精选区。
func (p *Post) IsFeatured() bool { return p.FeaturedAt != nil }

// HasTranslations 报告这篇是否属于某个译文分组。
func (p *Post) HasTranslations() bool { return p.TransKey != "" }

// NoIndex 报告这篇文章是否应当输出 <meta name="robots" content="noindex">。
//
// 未发布的内容必然 noindex；显式关掉 indexable 的也 noindex；
// 设置了站外 canonical 的转载文章同样不进索引——与其让 Google 判定
// 整站是采集站，不如主动放弃这一篇的收录。
func (p *Post) NoIndex() bool {
	return !p.IsPublished() || !p.Indexable || p.CanonicalURL != ""
}

// SearchHit 是一条检索结果。
//
// TitleHTML 与 Snippet 都是已转义、带 <mark> 的 HTML 片段，可直接输出。
// 命中可能只在标题里（正文没有该词），这时 Snippet 退化为普通摘要而
// TitleHTML 带高亮——所以两者要分开给，不能只给一个。
type SearchHit struct {
	Post      Post
	TitleHTML string
	Snippet   string
}

// Media 是一个上传的文件。
type Media struct {
	ID        int64
	UserID    int64
	SHA256    string
	Filename  string
	MIME      string
	Size      int64
	Path      string
	CreatedAt time.Time
}
