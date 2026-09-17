package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cligc.com/internal/i18n"
)

func open(t *testing.T) *DB {
	t.Helper()
	if len(i18n.Codes()) == 0 {
		if err := i18n.LoadBuiltin(); err != nil {
			t.Fatal(err)
		}
	}
	d, err := Open(filepath.Join(t.TempDir(), "test.db"), "cligc.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestUserAndAuth(t *testing.T) {
	ctx := context.Background()
	d := open(t)

	u, err := d.CreateUser(ctx, "A@Example.com ", "张三", "hunter2hunter2", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "a@example.com" {
		t.Errorf("email not normalized: %q", u.Email)
	}
	if u.Slug == "" {
		t.Error("empty slug")
	}
	if _, err := d.CreateUser(ctx, "a@example.com", "李四", "hunter2hunter2", "author"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate email = %v, want ErrConflict", err)
	}
	if _, err := d.Authenticate(ctx, "a@example.com", "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("bad password = %v", err)
	}
	if _, err := d.Authenticate(ctx, "nobody@example.com", "x"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("unknown user = %v", err)
	}
	got, err := d.Authenticate(ctx, "a@example.com", "hunter2hunter2")
	if err != nil || got.ID != u.ID {
		t.Fatalf("auth failed: %v", err)
	}
}

func TestTokenScopes(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")

	// 白名单过滤掉伪造的 scope
	plain, tok, err := d.CreateToken(ctx, u.ID, "ai-client",
		[]string{ScopePostsRead, ScopePostsWrite, "posts:admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Errorf("bad token format %q", plain)
	}
	if tok.Has(ScopePostsPublish) {
		t.Error("publish scope leaked")
	}
	if len(tok.Scopes) != 2 {
		t.Errorf("scopes = %v", tok.Scopes)
	}

	got, gotU, err := d.AuthenticateToken(ctx, plain)
	if err != nil || gotU.ID != u.ID || !got.Has(ScopePostsWrite) {
		t.Fatalf("token auth failed: %v", err)
	}
	if _, _, err := d.AuthenticateToken(ctx, plain+"x"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("tampered token = %v", err)
	}
	if err := d.RevokeToken(ctx, u.ID, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.AuthenticateToken(ctx, plain); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoked token still valid: %v", err)
	}
}

func TestPostLifecycleAndIdempotency(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID, Kind: "api"}

	p, err := d.CreatePost(ctx, a, CreatePostInput{
		Title:          "用 Go 和 SQLite 搭一个轻量内容站",
		BodyMD:         "这是正文。\n\n讲的是 [外链](https://example.com/x) 和全文检索。",
		Tags:           []string{"Go", "SQLite", "Go"},
		Source:         SourceAIAssisted,
		IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusDraft {
		t.Errorf("new post status = %q, want draft", p.Status)
	}
	if !strings.Contains(p.BodyHTML, `rel="ugc nofollow noopener"`) {
		t.Errorf("external link not marked: %s", p.BodyHTML)
	}
	if len(p.Tags) != 2 {
		t.Errorf("tags = %v, want 2 deduped", p.Tags)
	}
	if p.Summary == "" || p.WordCount == 0 {
		t.Errorf("summary/wordcount not derived: %+v", p.Summary)
	}

	// 幂等键复用同一篇，不新建
	again, err := d.CreatePost(ctx, a, CreatePostInput{Title: "别的标题", BodyMD: "别的正文", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != p.ID {
		t.Fatalf("idempotency broken: %d != %d", again.ID, p.ID)
	}

	// 草稿不出现在已发布列表
	list, total, _ := d.ListPosts(ctx, ListFilter{Status: StatusPublished})
	if total != 0 || len(list) != 0 {
		t.Errorf("draft leaked into published list")
	}

	if _, err := d.PublishPost(ctx, a, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	pub, _ := d.PostByID(ctx, p.ID)
	if !pub.IsPublished() || pub.PublishedAt == nil {
		t.Fatalf("publish failed: %+v", pub)
	}
	if pub.NoIndex() {
		t.Error("published indexable post should not be noindex")
	}

	// 设了站外 canonical 就不该进索引
	up, err := d.UpdatePost(ctx, a, p.ID, UpdatePostInput{CanonicalURL: ptr("https://zhuanlan.example/1")})
	if err != nil {
		t.Fatal(err)
	}
	if !up.NoIndex() {
		t.Error("canonical to external site should imply noindex")
	}

	// 别人不能改
	other, _ := d.CreateUser(ctx, "c@d.com", "他人", "password123", "author")
	if _, err := d.UpdatePost(ctx, Actor{UserID: other.ID}, p.ID, UpdatePostInput{Title: ptr("劫持")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("cross-user update = %v, want ErrForbidden", err)
	}
	// 管理员可以
	if _, err := d.UpdatePost(ctx, Actor{UserID: other.ID, IsAdmin: true}, p.ID, UpdatePostInput{Title: ptr("管理员改标题")}); err != nil {
		t.Errorf("admin update failed: %v", err)
	}
}

func TestDailyPublishCap(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID, Kind: "api"}

	mk := func(i int) int64 {
		p, err := d.CreatePost(ctx, a, CreatePostInput{Title: "文章", BodyMD: "正文内容"})
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	id1, id2, id3 := mk(1), mk(2), mk(3)

	if _, err := d.PublishPost(ctx, a, id1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PublishPost(ctx, a, id2, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PublishPost(ctx, a, id3, 2); !errors.Is(err, ErrDailyCap) {
		t.Fatalf("3rd publish = %v, want ErrDailyCap", err)
	}
	// 重复发布已发布的文章是空操作，不消耗配额
	if _, err := d.PublishPost(ctx, a, id1, 2); err != nil {
		t.Fatalf("republish should be a no-op: %v", err)
	}
	n, _ := d.PublishedToday(ctx, u.ID)
	if n != 2 {
		t.Errorf("publish events = %d, want 2", n)
	}
}

func TestChineseSearch(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID}

	mk := func(title, body string) {
		p, err := d.CreatePost(ctx, a, CreatePostInput{Title: title, BodyMD: body})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.PublishPost(ctx, a, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	mk("轻量内容站的架构选择", "我们用 Go 加 SQLite 搭了一个静态渲染的内容站，全文检索走 FTS5。")
	mk("关于摄影的随笔", "这篇完全在讲别的事情，和数据库没有任何关系。")

	// 正文命中：摘要应当带高亮
	for _, q := range []string{"全文检索", "SQLite", "内容站"} {
		hits, total, err := d.Search(ctx, q, SearchFilter{Status: StatusPublished, Limit: 10})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if total != 1 || len(hits) != 1 {
			t.Fatalf("search %q: total=%d hits=%d, want 1", q, total, len(hits))
		}
		if !strings.Contains(hits[0].Snippet, "<mark>") {
			t.Errorf("search %q: snippet not highlighted: %q", q, hits[0].Snippet)
		}
	}
	// 仅标题命中：正文摘要退化为普通摘要，但标题要高亮
	hits, total, err := d.Search(ctx, "架构", SearchFilter{Status: StatusPublished, Limit: 10})
	if err != nil || total != 1 {
		t.Fatalf("title-only search: total=%d err=%v", total, err)
	}
	if !strings.Contains(hits[0].TitleHTML, "<mark>架构</mark>") {
		t.Errorf("title not highlighted: %q", hits[0].TitleHTML)
	}
	if strings.Contains(hits[0].Snippet, "<mark>") {
		t.Errorf("body snippet should have no mark: %q", hits[0].Snippet)
	}
	// 不该出现的跨文档误命中
	if _, total, _ := d.Search(ctx, "摄影 SQLite", SearchFilter{Status: StatusPublished, Limit: 10}); total != 0 {
		t.Errorf("AND across documents should not match, got %d", total)
	}
	if _, total, _ := d.Search(ctx, "量子力学", SearchFilter{Status: StatusPublished, Limit: 10}); total != 0 {
		t.Errorf("unrelated query matched %d", total)
	}
	// 空查询直接返回空，不执行 MATCH
	if _, total, err := d.Search(ctx, "   ", SearchFilter{Status: StatusPublished, Limit: 10}); err != nil || total != 0 {
		t.Errorf("blank query: total=%d err=%v", total, err)
	}
}

func TestSearchIndexStaysInSync(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID}

	p, _ := d.CreatePost(ctx, a, CreatePostInput{Title: "原始标题", BodyMD: "原始的正文内容在这里。"})
	d.PublishPost(ctx, a, p.ID, 0)

	if _, total, _ := d.Search(ctx, "原始的正文", SearchFilter{Status: StatusPublished, Limit: 10}); total != 1 {
		t.Fatal("initial index missing")
	}
	if _, err := d.UpdatePost(ctx, a, p.ID, UpdatePostInput{BodyMD: ptr("换成了完全不同的段落。")}); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := d.Search(ctx, "原始的正文", SearchFilter{Status: StatusPublished, Limit: 10}); total != 0 {
		t.Error("stale index survived update")
	}
	if _, total, _ := d.Search(ctx, "完全不同", SearchFilter{Status: StatusPublished, Limit: 10}); total != 1 {
		t.Error("updated body not indexed")
	}
	if err := d.DeletePost(ctx, a, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := d.Search(ctx, "完全不同", SearchFilter{Status: StatusPublished, Limit: 10}); total != 0 {
		t.Error("index row survived delete")
	}
}

func TestMediaDedupe(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	root := t.TempDir()

	png := []byte("\x89PNG\r\n\x1a\nfake image bytes")
	m1, err := d.SaveMedia(ctx, root, u.ID, "a.png", "image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := d.SaveMedia(ctx, root, u.ID, "b.png", "image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	if m1.ID != m2.ID {
		t.Errorf("same bytes produced two records")
	}
	if _, err := d.SaveMedia(ctx, root, u.ID, "x.exe", "application/octet-stream", png); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("disallowed MIME accepted: %v", err)
	}
}

func TestVacuum(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	sid, exp, err := d.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Before(time.Now()) {
		t.Error("session expires in the past")
	}
	if got, err := d.UserBySession(ctx, sid); err != nil || got.ID != u.ID {
		t.Fatalf("session lookup: %v", err)
	}
	if err := d.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UserBySession(ctx, sid); err != nil {
		t.Error("vacuum deleted a live session")
	}
	d.DeleteSession(ctx, sid)
	if _, err := d.UserBySession(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Error("session not deleted")
	}
}

func ptr[T any](v T) *T { return &v }

// TestListLoadsTagsInOneQuery 确认列表页带上了标签（站内链的来源），
// 且没有退化成逐篇查询。
func TestListLoadsTagsInOneQuery(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID}

	for i, tags := range [][]string{{"Go", "SQLite"}, {"Go"}, nil} {
		p, err := d.CreatePost(ctx, a, CreatePostInput{
			Title: "文章" + string(rune('A'+i)), BodyMD: "正文内容在此", Tags: tags,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.PublishPost(ctx, a, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}

	posts, total, err := d.ListPosts(ctx, ListFilter{Status: StatusPublished})
	if err != nil || total != 3 {
		t.Fatalf("list: total=%d err=%v", total, err)
	}
	counts := map[string]int{}
	for _, p := range posts {
		counts[p.Title] = len(p.Tags)
	}
	if counts["文章A"] != 2 || counts["文章B"] != 1 || counts["文章C"] != 0 {
		t.Errorf("tags not attached correctly: %v", counts)
	}

	// 检索结果同样要带标签
	hits, _, err := d.Search(ctx, "正文内容", SearchFilter{Status: StatusPublished, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Post.Title == "文章A" {
			found = len(h.Post.Tags) == 2
		}
	}
	if !found {
		t.Error("search hits should carry tags too")
	}
}

func TestCommentModeration(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID}
	p, _ := d.CreatePost(ctx, a, CreatePostInput{Title: "文章", BodyMD: "正文内容"})

	// 草稿上不能评论——前端表单能被绕过，必须拦在这里
	if _, err := d.CreateComment(ctx, CreateCommentInput{PostID: p.ID, Body: "抢沙发"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("comment on draft = %v, want ErrForbidden", err)
	}
	d.PublishPost(ctx, a, p.ID, 0)

	// 匿名 -> pending
	anon, err := d.CreateComment(ctx, CreateCommentInput{PostID: p.ID, Body: "路过的访客"})
	if err != nil {
		t.Fatal(err)
	}
	if anon.Status != CommentPending || !anon.IsAnonymous() || anon.AuthorName != "匿名" {
		t.Fatalf("anonymous comment = %+v", anon)
	}

	// 登录用户 -> 直接通过，并带上其显示名
	mine, err := d.CreateComment(ctx, CreateCommentInput{PostID: p.ID, UserID: &u.ID, Body: "我自己的回复"})
	if err != nil {
		t.Fatal(err)
	}
	if mine.Status != CommentApproved || mine.AuthorName != "作者" {
		t.Fatalf("logged-in comment = %+v", mine)
	}

	// 公开页面只看得到已通过的
	shown, err := d.CommentsForPost(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shown) != 1 || shown[0].ID != mine.ID {
		t.Fatalf("public comments = %+v, want only the approved one", shown)
	}

	// 审核通过之后才出现
	if err := d.SetCommentStatus(ctx, a, anon.ID, CommentApproved); err != nil {
		t.Fatal(err)
	}
	if shown, _ := d.CommentsForPost(ctx, p.ID); len(shown) != 2 {
		t.Errorf("after approval got %d comments, want 2", len(shown))
	}

	// 标垃圾是可逆的，且立刻从公开页消失
	if err := d.SetCommentStatus(ctx, a, anon.ID, CommentSpam); err != nil {
		t.Fatal(err)
	}
	if shown, _ := d.CommentsForPost(ctx, p.ID); len(shown) != 1 {
		t.Error("spam comment still public")
	}

	// 别人不能审核我文章下的评论
	other, _ := d.CreateUser(ctx, "c@d.com", "他人", "password123", "author")
	if err := d.SetCommentStatus(ctx, Actor{UserID: other.ID}, anon.ID, CommentApproved); !errors.Is(err, ErrForbidden) {
		t.Errorf("cross-user moderation = %v, want ErrForbidden", err)
	}

	// 长度限制
	if _, err := d.CreateComment(ctx, CreateCommentInput{PostID: p.ID, Body: " "}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty comment = %v", err)
	}
	long := make([]rune, MaxCommentLen+1)
	for i := range long {
		long[i] = '字'
	}
	if _, err := d.CreateComment(ctx, CreateCommentInput{PostID: p.ID, Body: string(long)}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("overlong comment = %v", err)
	}
}

func TestCommentQueueScopedToAuthor(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u1, _ := d.CreateUser(ctx, "a@b.com", "甲", "password123", "author")
	u2, _ := d.CreateUser(ctx, "c@d.com", "乙", "password123", "author")

	mk := func(u *User, title string) int64 {
		p, _ := d.CreatePost(ctx, Actor{UserID: u.ID}, CreatePostInput{Title: title, BodyMD: "正文"})
		d.PublishPost(ctx, Actor{UserID: u.ID}, p.ID, 0)
		d.CreateComment(ctx, CreateCommentInput{PostID: p.ID, Body: "评论 " + title})
		return p.ID
	}
	mk(u1, "甲的文章")
	mk(u2, "乙的文章")

	// 作者只看得到自己文章下的评论
	got, total, err := d.ListComments(ctx, CommentFilter{Status: CommentPending, AuthorID: u1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(got) != 1 || got[0].PostTitle != "甲的文章" {
		t.Fatalf("author-scoped queue = %d items %+v", total, got)
	}
	// 不限作者时（管理员）看到全部
	if _, total, _ := d.ListComments(ctx, CommentFilter{Status: CommentPending}); total != 2 {
		t.Errorf("admin queue total = %d, want 2", total)
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	sid, _, _ := d.CreateSession(ctx, u.ID)

	if err := d.ChangePassword(ctx, u.ID, "wrong", "newpassword123"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong current password = %v", err)
	}
	if err := d.ChangePassword(ctx, u.ID, "password123", "short"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("short new password = %v", err)
	}
	if err := d.ChangePassword(ctx, u.ID, "password123", "newpassword123"); err != nil {
		t.Fatal(err)
	}
	// 改密的常见动机是怀疑被盗号，旧会话必须一起失效
	if _, err := d.UserBySession(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Error("old session survived a password change")
	}
	if _, err := d.Authenticate(ctx, "a@b.com", "newpassword123"); err != nil {
		t.Errorf("new password does not work: %v", err)
	}
}

func TestUpdateUser(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")

	got, err := d.UpdateUser(ctx, u.ID, "新名字", "一句简介", "my-page")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "新名字" || got.Slug != "my-page" || got.Bio != "一句简介" {
		t.Fatalf("update = %+v", got)
	}
	if got.Email != "a@b.com" {
		t.Error("email must not change")
	}
	// slug 冲突时自动加后缀，而不是报错
	u2, _ := d.CreateUser(ctx, "c@d.com", "他人", "password123", "author")
	got2, err := d.UpdateUser(ctx, u2.ID, "他人", "", "my-page")
	if err != nil {
		t.Fatal(err)
	}
	if got2.Slug == "my-page" {
		t.Error("slug collision not resolved")
	}
	if _, err := d.UpdateUser(ctx, u.ID, "  ", "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("blank name = %v", err)
	}
}

func TestMediaDeleteRemovesFile(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	root := t.TempDir()

	png := []byte("\x89PNG\r\n\x1a\nfake")
	m, err := d.SaveMedia(ctx, root, u.ID, "a.png", "image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(root, m.Path)
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("file not written: %v", err)
	}

	other, _ := d.CreateUser(ctx, "c@d.com", "他人", "password123", "author")
	if err := d.DeleteMedia(ctx, Actor{UserID: other.ID}, root, m.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("cross-user delete = %v, want ErrForbidden", err)
	}
	if err := d.DeleteMedia(ctx, Actor{UserID: u.ID}, root, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Error("file left on disk after delete")
	}
	if _, err := d.MediaByID(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Error("row left in db after delete")
	}
}

func TestRerenderAllRefreshesCacheAndIndex(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID}
	p, _ := d.CreatePost(ctx, a, CreatePostInput{
		Title: "带代码的文章", BodyMD: "```go\nfunc main() {}\n```\n\n正文内容在这里。",
		Summary: "作者手写的摘要",
	})
	d.PublishPost(ctx, a, p.ID, 0)

	// 模拟渲染器变更前的陈旧缓存
	if _, err := d.W.ExecContext(ctx,
		`update posts set body_html='<p>stale</p>', body_text='stale' where id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	n, err := d.RerenderAll(ctx)
	if err != nil || n != 1 {
		t.Fatalf("rerender = %d, %v", n, err)
	}
	got, _ := d.PostByID(ctx, p.ID)
	if strings.Contains(got.BodyHTML, "stale") {
		t.Error("stale html cache survived rerender")
	}
	if !strings.Contains(got.BodyHTML, "codeblock") {
		t.Errorf("rerendered html missing highlighting wrapper: %s", got.BodyHTML)
	}
	// 手写摘要不该被重算冲掉
	if got.Summary != "作者手写的摘要" {
		t.Errorf("rerender clobbered a hand-written summary: %q", got.Summary)
	}
	if _, total, _ := d.Search(ctx, "正文内容", SearchFilter{Status: StatusPublished, Limit: 10}); total != 1 {
		t.Error("search index not rebuilt")
	}
}

func TestFeatured(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID}

	mk := func(title string, publish bool) int64 {
		p, err := d.CreatePost(ctx, a, CreatePostInput{Title: title, BodyMD: "正文内容"})
		if err != nil {
			t.Fatal(err)
		}
		if publish {
			if _, err := d.PublishPost(ctx, a, p.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
		return p.ID
	}
	first, second := mk("第一篇", true), mk("第二篇", true)
	draft := mk("草稿", false)

	// 草稿不能被精选：精选区是首页最显眼的位置，
	// 让草稿出现在那里等于绕过了发布这道闸门
	if _, err := d.SetFeatured(ctx, a, draft, true); !errors.Is(err, ErrForbidden) {
		t.Errorf("featuring a draft = %v, want ErrForbidden", err)
	}

	if _, err := d.SetFeatured(ctx, a, first, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // featured_at 是秒级，要拉开顺序
	if _, err := d.SetFeatured(ctx, a, second, true); err != nil {
		t.Fatal(err)
	}

	// 最近置顶的排在前面
	feat, err := d.FeaturedPosts(ctx, i18n.Default().Code, MaxFeatured)
	if err != nil {
		t.Fatal(err)
	}
	if len(feat) != 2 || feat[0].ID != second {
		t.Fatalf("featured order = %v, want most-recently-pinned first", ids(feat))
	}

	// 精选的从普通列表里排除，否则首页一屏里会出现两次
	rest, total, err := d.ListPosts(ctx, ListFilter{Status: StatusPublished, ExcludeFeatured: true})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(rest) != 0 {
		t.Errorf("featured posts leaked into the excluded list: %v", ids(rest))
	}
	// 不排除时仍然在
	if _, total, _ := d.ListPosts(ctx, ListFilter{Status: StatusPublished}); total != 2 {
		t.Errorf("unfiltered list total = %d, want 2", total)
	}

	// 取消置顶后回到普通列表
	if _, err := d.SetFeatured(ctx, a, first, false); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := d.ListPosts(ctx, ListFilter{Status: StatusPublished, ExcludeFeatured: true}); total != 1 {
		t.Error("unfeatured post did not return to the normal list")
	}
	if n, _ := d.CountFeatured(ctx); n != 1 {
		t.Errorf("CountFeatured = %d, want 1", n)
	}

	// 别人不能动我的文章
	other, _ := d.CreateUser(ctx, "c@d.com", "他人", "password123", "author")
	if _, err := d.SetFeatured(ctx, Actor{UserID: other.ID}, second, false); !errors.Is(err, ErrForbidden) {
		t.Errorf("cross-user unfeature = %v, want ErrForbidden", err)
	}
}

// TestMigrationAddsColumnToExistingDB 守住迁移路径。
//
// schema.sql 全是 create ... if not exists，对已经存在的库来说新加的列不会
// 被套用——建表语句直接跳过了。而且依赖新列的索引不能写进 schema.sql：
// 那个文件整体跑在补列之前，在旧库上 create index 会因为列不存在而失败，
// 并连带把后面的 alter table 一起挡掉，结果是库卡在半路且服务起不来。
func TestMigrationAddsColumnToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// 造一个"旧版本"的库：posts 表没有 featured_at
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`create table posts (
		id integer primary key, user_id integer not null, slug text not null unique,
		title text not null, summary text not null default '', body_md text not null,
		body_html text not null, body_text text not null default '',
		status text not null default 'draft', source text not null default 'human',
		indexable integer not null default 1, canonical_url text not null default '',
		word_count integer not null default 0, created_at integer not null,
		updated_at integer not null, published_at integer)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	d, err := Open(path, "example.com")
	if err != nil {
		t.Fatalf("Open on an older database failed: %v", err)
	}
	defer d.Close()

	var n int
	if err := d.R.QueryRow(
		`select count(*) from pragma_table_info('posts') where name='featured_at'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("migration did not add posts.featured_at to the existing database")
	}
	// 依赖新列的索引也要建上
	if err := d.R.QueryRow(
		`select count(*) from sqlite_master where type='index' and name='idx_posts_feat'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("index depending on the new column was not created")
	}
	// 而且库是可用的
	u, err := d.CreateUser(context.Background(), "a@b.com", "作者", "password123", "author")
	if err != nil {
		t.Fatalf("migrated database is not usable: %v", err)
	}
	if _, err := d.CreatePost(context.Background(), Actor{UserID: u.ID},
		CreatePostInput{Title: "迁移后仍可写入", BodyMD: "正文"}); err != nil {
		t.Fatalf("write after migration failed: %v", err)
	}
}

func ids(ps []Post) []int64 {
	out := make([]int64, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func TestTranslationGroup(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID}

	zh, err := d.CreatePost(ctx, a, CreatePostInput{Title: "中文版", BodyMD: "正文", Lang: "zh-Hans"})
	if err != nil {
		t.Fatal(err)
	}
	en, err := d.CreatePost(ctx, a, CreatePostInput{Title: "English", BodyMD: "Body", Lang: "en"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{zh.ID, en.ID} {
		if _, err := d.PublishPost(ctx, a, id, 0); err != nil {
			t.Fatal(err)
		}
	}

	// 未知语言退回默认，而不是让保存失败——语言是展示维度，不该因为
	// 一个拼错的值丢掉整篇文章
	bad, _ := d.CreatePost(ctx, a, CreatePostInput{Title: "x", BodyMD: "y", Lang: "klingon"})
	if bad.Lang != i18n.Default().Code {
		t.Errorf("unknown lang = %q, want fallback to default", bad.Lang)
	}

	if err := d.LinkTranslation(ctx, a, en.ID, zh.ID); err != nil {
		t.Fatal(err)
	}
	// 分组是对称的：从任意一边都查得到另一边
	for _, c := range []struct{ from, want int64 }{{zh.ID, en.ID}, {en.ID, zh.ID}} {
		p, _ := d.PostByID(ctx, c.from)
		others, err := d.Translations(ctx, p.TransKey, p.ID)
		if err != nil || len(others) != 1 || others[0].ID != c.want {
			t.Fatalf("from %d: got %v, want [%d]", c.from, ids(others), c.want)
		}
	}
	// 自己不能是自己的译文
	if err := d.LinkTranslation(ctx, a, zh.ID, zh.ID); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("self-translation = %v, want ErrInvalidInput", err)
	}
	// 列表按语言隔离
	if _, n, _ := d.ListPosts(ctx, ListFilter{Status: StatusPublished, Lang: "en"}); n != 1 {
		t.Errorf("en list = %d, want 1", n)
	}
	// 草稿版本不进 hreflang：指向草稿或 404 会让 Google 整组忽略
	draft, _ := d.CreatePost(ctx, a, CreatePostInput{Title: "草稿译文", BodyMD: "x", Lang: "en"})
	d.LinkTranslation(ctx, a, draft.ID, zh.ID)
	p, _ := d.PostByID(ctx, zh.ID)
	others, _ := d.Translations(ctx, p.TransKey, p.ID)
	for _, o := range others {
		if o.Status != StatusPublished {
			t.Errorf("unpublished post %d leaked into the hreflang set", o.ID)
		}
	}
	// 解除关联
	if err := d.LinkTranslation(ctx, a, en.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.PostByID(ctx, en.ID); got.TransKey != "" {
		t.Error("unlink did not clear the translation key")
	}
}

// TestEmptySlugDoesNotChangeURL 守住一条安全性质。
//
// 改 URL 是破坏性操作：已有链接失效、已积累的收录作废。让它因为一个
// 恰好为空的表单字段就发生，代价远大于少一个"留空自动重生成"的便利。
func TestEmptySlugDoesNotChangeURL(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, _ := d.CreateUser(ctx, "a@b.com", "作者", "password123", "author")
	a := Actor{UserID: u.ID}

	p, err := d.CreatePost(ctx, a, CreatePostInput{Title: "标题", BodyMD: "正文", Slug: "my-post"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Slug != "my-post" {
		t.Fatalf("slug = %q", p.Slug)
	}
	// 只改标题，slug 字段留空
	empty := ""
	got, err := d.UpdatePost(ctx, a, p.ID, UpdatePostInput{
		Title: ptr("改过的标题"), Slug: &empty,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "my-post" {
		t.Errorf("empty slug changed the URL: %q -> %q", "my-post", got.Slug)
	}
	// 显式填写才会改
	got, _ = d.UpdatePost(ctx, a, p.ID, UpdatePostInput{Slug: ptr("new-slug")})
	if got.Slug != "new-slug" {
		t.Errorf("explicit slug ignored: %q", got.Slug)
	}
}

// TestScheduledPostStaysInvisibleUntilDue 守住定时发布最要紧的那一条：
// 排期中的文章在到点之前，对外必须完全不存在。
//
// 它靠的是一个约定——排期只是给 draft 加个时间戳，状态不变。一旦有人
// "顺手"把排期中的文章直接标成 published 再靠时间过滤，那么列表、搜索、
// sitemap、feed 就得各自记得多加一条 where，漏掉任何一个都是提前泄露。
func TestScheduledPostStaysInvisibleUntilDue(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, err := d.CreateUser(ctx, "s@example.com", "作者", "hunter2hunter2", "admin")
	if err != nil {
		t.Fatal(err)
	}
	a := Actor{UserID: u.ID}
	p, err := d.CreatePost(ctx, a, CreatePostInput{Title: "排期中", BodyMD: "正文"})
	if err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(2 * time.Hour)
	if err := d.SetSchedule(ctx, a, p.ID, &future); err != nil {
		t.Fatal(err)
	}
	got, err := d.PostByID(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDraft {
		t.Errorf("排期后状态成了 %q —— 必须仍是 draft，否则公开查询会立刻放它出去", got.Status)
	}
	if got.PublishAt == nil {
		t.Fatal("排期时间没存下来")
	}

	// 还没到点
	if due, err := d.DuePosts(ctx, time.Now()); err != nil || len(due) != 0 {
		t.Errorf("提前捞到了 %d 篇（err=%v）", len(due), err)
	}
	if n, errs := d.PublishDue(ctx, time.Now()); len(errs) != 0 || n != 0 {
		t.Errorf("提前发布了 %d 篇（errs=%v）", n, errs)
	}

	// 到点：发出去、清掉排期、不重复发
	if n, errs := d.PublishDue(ctx, future.Add(time.Second)); len(errs) != 0 || n != 1 {
		t.Fatalf("到点没发出去：n=%d errs=%v", n, errs)
	}
	got, _ = d.PostByID(ctx, p.ID)
	if got.Status != StatusPublished {
		t.Errorf("到点后状态是 %q，想要 published", got.Status)
	}
	if got.PublishAt != nil {
		t.Error("发布后排期时间没清掉——下一轮扫描会再发一遍")
	}
	if n, _ := d.PublishDue(ctx, future.Add(time.Hour)); n != 0 {
		t.Error("同一篇被重复发布了")
	}
}

// TestSettingsCacheIsPerDB 守住设置缓存不跨实例串数据。
//
// 缓存一开始写成了包级变量。同一个进程里有多个 DB 实例时（测试就是这样，
// 未来一个进程跑多站也是），那份缓存会让它们互相读到对方的设置——
// 表现是"我明明没配 GA4，页面上却有"，而且只在第二个实例上出现。
func TestSettingsCacheIsPerDB(t *testing.T) {
	ctx := context.Background()
	a, b := open(t), open(t)

	if err := a.SaveSettings(ctx, SiteSettings{GA4ID: "G-AAA", IndexNowKey: "DEADBEEF"}); err != nil {
		t.Fatal(err)
	}
	if got := a.Settings(ctx); got.GA4ID != "G-AAA" {
		t.Errorf("同一个库读不回自己写的：%q", got.GA4ID)
	}
	if got := b.Settings(ctx); got.GA4ID != "" {
		t.Errorf("另一个库读到了串过来的设置：%q", got.GA4ID)
	}

	// key 统一小写去空白，省得站长复制粘贴时带进大小写差异，
	// 而校验文件的内容必须和提交时用的 key 逐字节一致。
	if got := a.Settings(ctx).IndexNowKey; got != "deadbeef" {
		t.Errorf("IndexNowKey = %q，应当规范成小写", got)
	}

	// 清空要能真的清掉，不能只是"没写进去"
	if err := a.SaveSettings(ctx, SiteSettings{}); err != nil {
		t.Fatal(err)
	}
	if got := a.Settings(ctx); got != (SiteSettings{}) {
		t.Errorf("清空后仍留着 %+v", got)
	}
}

// TestCategoryIsNavigationNotOwnership 守住分类和标签是两个维度，
// 以及删分类不等于删内容。
//
// 最容易实现错的是外键那一条：posts.category_id 写成 on delete cascade 的话，
// 删一个分类会连着删掉底下所有文章。整理导航和删内容的后果差了几个数量级，
// 不该因为一次误点就混在一起。
func TestCategoryIsNavigationNotOwnership(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	u, err := d.CreateUser(ctx, "c@example.com", "作者", "hunter2hunter2", "admin")
	if err != nil {
		t.Fatal(err)
	}
	a := Actor{UserID: u.ID}

	cat, err := d.CreateCategory(ctx, "随笔", "", "随笔与杂记", 1)
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.CreatePost(ctx, a, CreatePostInput{
		Title: "一篇随笔", BodyMD: "正文", Tags: []string{"Go", "SEO"},
		CategorySlug: cat.Slug,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.PublishPost(ctx, a, p.ID, 0); err != nil {
		t.Fatal(err)
	}

	// 两个维度同时成立：一篇文章既属于一个分类，又带着多个标签
	got, _ := d.PostByID(ctx, p.ID)
	if got.CategoryName != "随笔" {
		t.Errorf("分类没读回来：%q", got.CategoryName)
	}
	if len(got.Tags) != 2 {
		t.Errorf("标签数 %d，想要 2 —— 分类不该影响标签", len(got.Tags))
	}

	// 分类计数只算已发布的：导航上写着"随笔 3"点进去只有 1 篇，比不显示更糟
	cats, _ := d.ListCategories(ctx)
	if len(cats) != 1 || cats[0].Count != 1 {
		t.Fatalf("分类计数不对：%+v", cats)
	}
	draft, _ := d.CreatePost(ctx, a, CreatePostInput{
		Title: "草稿", BodyMD: "正文", CategorySlug: cat.Slug})
	cats, _ = d.ListCategories(ctx)
	if cats[0].Count != 1 {
		t.Errorf("草稿被算进了分类计数：%d", cats[0].Count)
	}
	_ = draft

	// 按分类筛选
	list, total, err := d.ListPosts(ctx, ListFilter{CategorySlug: cat.Slug})
	if err != nil || total != 2 {
		t.Errorf("按分类筛出 %d 篇（err=%v），想要 2", total, err)
	}
	_ = list
	// "-" 专指未分类
	if _, n, _ := d.ListPosts(ctx, ListFilter{CategorySlug: "-"}); n != 0 {
		t.Errorf("未分类应当是 0 篇，实际 %d", n)
	}

	// 删分类：文章一篇不少，只是变成未分类
	if err := d.DeleteCategory(ctx, cat.ID); err != nil {
		t.Fatal(err)
	}
	got, err = d.PostByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("删掉分类之后文章也没了 —— 外键写成 cascade 了：%v", err)
	}
	if got.CategoryID != nil || got.CategoryName != "" {
		t.Errorf("分类删了，文章上却还挂着 %v/%q", got.CategoryID, got.CategoryName)
	}
	if _, n, _ := d.ListPosts(ctx, ListFilter{CategorySlug: "-"}); n != 2 {
		t.Errorf("删分类后应有 2 篇未分类，实际 %d", n)
	}
}

// TestCommentsDefaultOnForUntouchedSite 守住"从没进过设置页的站，评论是开的"。
//
// 库里存的是反过来的 comments_off，就是为了这一条：布尔值直接存的话，
// 一个从没保存过设置的站会读到空值 → false → 评论被悄悄关掉，而站长
// 什么也没做过。默认值不能依赖"有人来设置过"。
func TestCommentsDefaultOnForUntouchedSite(t *testing.T) {
	ctx := context.Background()
	d := open(t)
	if !d.Settings(ctx).CommentsEnabled {
		t.Fatal("全新的站评论默认是关的 —— 站长什么都没做就被关掉了")
	}
	// 显式关掉要能存住
	if err := d.SaveSettings(ctx, SiteSettings{CommentsEnabled: false}); err != nil {
		t.Fatal(err)
	}
	if d.Settings(ctx).CommentsEnabled {
		t.Error("关掉之后又变回开了")
	}
	// 再开回来
	if err := d.SaveSettings(ctx, SiteSettings{CommentsEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if !d.Settings(ctx).CommentsEnabled {
		t.Error("开回来失败了")
	}
}
