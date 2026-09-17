-- 所有时间戳统一存 Unix 秒（integer）。SQLite 没有原生时间类型，
-- 存整数比存字符串省空间、好比较、无时区歧义。

create table if not exists users (
  id            integer primary key,
  email         text    not null unique,
  name          text    not null,
  slug          text    not null unique,
  bio           text    not null default '',
  password_hash text    not null,
  role          text    not null default 'author',   -- admin | author
  created_at    integer not null
);

-- 网页端登录会话（人用）。API token 是另一套，见 api_tokens。
create table if not exists sessions (
  id         text    primary key,
  user_id    integer not null references users(id) on delete cascade,
  created_at integer not null,
  expires_at integer not null
);
create index if not exists idx_sessions_expires on sessions(expires_at);

-- API token（AI 客户端用）。只存 sha256，明文仅在创建时返回一次。
create table if not exists api_tokens (
  id           integer primary key,
  user_id      integer not null references users(id) on delete cascade,
  name         text    not null,
  prefix       text    not null,          -- 仅用于界面上辨认，非密文
  token_hash   text    not null unique,
  scopes       text    not null,          -- 逗号分隔，见 store.Scope*
  created_at   integer not null,
  last_used_at integer,
  expires_at   integer,
  revoked_at   integer
);
create index if not exists idx_tokens_user on api_tokens(user_id);

create table if not exists posts (
  id            integer primary key,
  user_id       integer not null references users(id),
  slug          text    not null unique,
  title         text    not null,
  summary       text    not null default '',
  body_md       text    not null,
  body_html     text    not null,          -- 渲染缓存，写时生成
  body_text     text    not null default '', -- 纯文本，用于检索结果的摘要高亮
  toc_json      text    not null default '', -- 文章大纲（h2/h3），和 body_html 一样是派生物
  status        text    not null default 'draft',  -- draft | published | archived
  source        text    not null default 'human',  -- human | ai-assisted | ai-generated
  indexable     integer not null default 1,        -- 0 则输出 noindex
  canonical_url text    not null default '',       -- 站外首发时指回原站
  word_count    integer not null default 0,
  -- 精选。存时间戳而不是布尔值：非空即表示精选，同时天然给出排序依据
  -- （刚置顶的排在前面），不需要再加一个 order 字段。
  featured_at   integer,
  -- 语言与译文分组。
  -- translation_key 让同一内容的各语言版本共享一个值，是**对称**的分组而不是
  -- 主从关系：加第三个语种时不用决定"它是谁的孩子"，删掉任何一篇也不会让
  -- 其余版本变成孤儿。现实中英文版写得比中文版好是常事，没有哪个版本天然更"正"。
  lang            text not null default 'zh-Hans',
  translation_key text not null default '',
  created_at    integer not null,
  updated_at    integer not null,
  published_at  integer,
  -- 定时发布：到点由后台任务改成 published。设了它的文章仍然是 draft，
  -- 所以公开列表、sitemap、feed 全都自动看不到它，不需要额外的过滤。
  publish_at    integer
);
create index if not exists idx_posts_pub  on posts(status, published_at desc);
-- 注意：依赖后加列（如 featured_at）的索引不能写在这里。这个文件整体跑在
-- migrate() 之前，那时旧库上的新列还不存在，create index 会直接失败，
-- 连带把后面的加列也挡住。这类索引放在 migrate() 的第二阶段。
create index if not exists idx_posts_user on posts(user_id, updated_at desc);

create table if not exists tags (
  id   integer primary key,
  slug text not null unique,
  name text not null
);
-- 分类：和标签是两个维度。
--
--   分类 = 结构。少、稳定、一篇一个、决定站点有哪几个板块，用来导航。
--   标签 = 描述。多、扁平、一篇多个、用来发现相关内容。
--
-- 所以分类单独一张表而不是复用 tags：它需要手动排序（导航顺序该由人定，
-- 不是按字母），需要在 posts 上有一个明确的外键（一篇只能属于一个板块），
-- 而这两件事塞进 tags 都是别扭的。
create table if not exists categories (
  id   integer primary key,
  slug text not null unique,
  name text not null,
  -- 导航顺序。相同时按 name 兜底，保证结果稳定。
  sort integer not null default 0
);

create table if not exists post_tags (
  post_id integer not null references posts(id) on delete cascade,
  tag_id  integer not null references tags(id)  on delete cascade,
  primary key (post_id, tag_id)
);
create index if not exists idx_post_tags_tag on post_tags(tag_id, post_id);

-- 全文索引。存的是 tokenize 包切好的空格分隔 token，不是原文；
-- 摘要高亮由调用方基于原文另算。rowid 与 posts.id 对齐。
create virtual table if not exists post_fts using fts5(
  title, body, tokenize="unicode61 remove_diacritics 2"
);

-- 幂等键。AI 客户端会重试，没有这张表迟早出现重复文章。
create table if not exists idempotency (
  user_id    integer not null,
  key        text    not null,
  post_id    integer not null,
  created_at integer not null,
  primary key (user_id, key)
);
create index if not exists idx_idem_created on idempotency(created_at);

-- 发布事件：既是审计流水，也是每日发布上限的计数依据。
create table if not exists publish_events (
  id         integer primary key,
  post_id    integer not null,
  user_id    integer not null,
  actor      text    not null,     -- web | api
  day        text    not null,     -- YYYY-MM-DD（UTC）
  created_at integer not null
);
create index if not exists idx_publish_day on publish_events(user_id, day);

create table if not exists media (
  id         integer primary key,
  user_id    integer not null references users(id),
  sha256     text    not null unique,
  filename   text    not null,
  mime       text    not null,
  size       integer not null,
  path       text    not null,
  created_at integer not null
);

-- 评论。
--
-- 正文按纯文本存，渲染时整体转义且不做任何自动链接——这个站的整个论点
-- 就是"别变成外链农场的宿主"，而评论区是最经典的灌链入口。允许 Markdown
-- 或自动识别 URL 都会把这道口子打开，代价远大于收益。
--
-- user_id 为空表示匿名访客。登录用户的评论直接通过，匿名评论一律进
-- pending 队列等人工审核。
create table if not exists comments (
  id          integer primary key,
  post_id     integer not null references posts(id) on delete cascade,
  user_id     integer          references users(id) on delete set null,
  author_name text    not null,
  body        text    not null,
  status      text    not null default 'pending',  -- pending | approved | spam
  created_at  integer not null
);
create index if not exists idx_comments_post on comments(post_id, status, created_at);
create index if not exists idx_comments_mod  on comments(status, created_at desc);

-- 站点设置：站长在后台填、随时可改的那些值。
--
-- 和命令行参数（-addr / -db / -base-url）刻意分开：那些是部署时就定死的
-- 基础设施参数，而这里是从各家网页控制台复制粘贴过来的凭据，改一次就要
-- 重启一次服务是不合理的流程。
-- 按内容去重的查询要能走索引：一篇文章下评论上千之后，
-- 每次提交都全表扫会把提交路径拖慢到肉眼可见。
create index if not exists idx_comments_dup on comments(post_id, status);

create table if not exists settings (
  key        text primary key,
  value      text not null default '',
  updated_at integer not null
);
