# 任务：将 Rails 博客系统（Rables）的数据无损迁移到 Go 项目

## 背景

我们正在把一个 Rails 8.1 个人博客系统重写为 Go。Rails 侧的全部状态 = **一个 SQLite 数据库文件** + **一个 S3 bucket**（存放全部上传图片）。你需要在 Go 项目中实现数据的导入、存储和访问。迁移必须**无损**：不允许任何形式的内容转换损耗（特别禁止 HTML→Markdown 这类有损转换作为存量数据的迁移路径）。

Rails 侧的导出脚本会产出一个「迁移包」（目录结构见下文 §3），你的工作是消费这个迁移包。如果迁移包尚未就绪，你可以先按 §3 的规范实现导入器和校验器，并用构造的样例数据测试。

## 1. 源系统事实（Rails 侧）

### 1.1 内容模型

文章（`articles` 表）有两种内容形态，由 `content_type` 列区分：

- `rich_text`：ActionText 富文本，正文存在 `action_text_rich_texts` 表（`record_type='Article' AND name='content' AND record_id=articles.id`）的 `body` 列。这是 Rails 私有的 canonical HTML，其中图片是 `<action-text-attachment sgid="...">` 占位标签，**离开 Rails 无法直接渲染**（sgid 需要 Rails 的 secret_key_base 验签）。
- `html`：正文存在 `articles.html_content` 列，是已渲染的 HTML，图片为 `<img src="/rails/active_storage/blobs/redirect/<signed_id>/<filename>">` 或外部 http URL。

页面（`pages` 表）结构与文章相同（同样有 `content_type` 和 ActionText 正文）。

**这就是为什么导出必须在 Rails 侧完成渲染**：Rails 侧会把每篇文章渲染为最终 HTML（应用 `Article#rendered_content` 的完整逻辑），并把其中的 `/rails/active_storage/...` 图片 URL 重写为新路径。Go 侧收到的永远是可直接渲染的成品 HTML。

### 1.2 枚举映射

- 文章/页面 `status`（整数）：`0=draft, 1=publish, 2=schedule, 3=trash, 4=shared`
- 评论 `status`（整数）：`0=pending, 1=approved, 2=rejected`
- 注意：**文章的发布时间就是 `created_at`**（定时发布会把 `created_at` 改写为预定时间），没有单独的 published_at 列。

### 1.3 图片/文件存储（ActiveStorage + S3）

- S3 bucket 里所有对象**平铺在根目录**，object key = `active_storage_blobs.key`（约 28 字符随机串，**无目录、无扩展名**）。
- `active_storage_blobs` 表：`key`（唯一）、`filename`（原始文件名）、`content_type`、`byte_size`、`checksum`（**文件内容的 base64 编码 MD5**，用于校验）、`service_name`（`s3` 或 `local`）。
- 若存在 `service_name='local'` 的 blob（历史遗留），文件在 Rails 项目本地 `storage/<key[0:2]>/<key[2:4]>/<key>` 两级目录下，导出时会一并收编。
- `active_storage_variant_records` 里的缩略图是运行时缓存，**不迁移**。
- 老内容里可能还有直接引用外部 http URL 的图片，导出时会下载收编为本地文件。

### 1.4 时间戳格式

SQLite 中的 datetime 为 UTC 字符串（`YYYY-MM-DD HH:MM:SS.ffffff`），导入时注意解析和时区处理。

## 2. 核心设计决策（已实现共识，不要偏离）

1. **Go 侧内容模型采用双格式**：`content_type ∈ {html, markdown}`。全部迁移文章为 `html` 类型，存储成品 HTML 直接渲染；`markdown` 类型只用于未来新写的文章（可用 goldmark 渲染）。这与 Rails 原系统的设计一致。
2. **双份正文**：迁移包中每篇文章同时包含「成品 HTML」（直接使用）和「原始 source」（ActionText canonical body 或 html_content 原文，原样存档）。原始 source 必须入库或妥善存档，保证永远可回溯。
3. **导出只做搬运和 URL 重写，不做内容解释**：枚举映射在导入侧做；HTML 结构不做任何"清理"或"优化"。

## 3. 迁移包规范（Rails 导出脚本产出的契约）

迁移包由 Rails 侧的导出任务生成（在 Rails 项目根目录执行）：

```bash
bin/rails site:export                      # 输出到 tmp/site_export_<时间戳>/
bin/rails "site:export[/path/to/backup]"   # 指定输出目录（zsh 下需要引号）
```

也可以在 Admin → Migrate → Export 页面点击 "Export for Go Migration" 后台执行（产物打包为 ZIP 供下载）。导出实现：`app/models/site_export.rb`（`SiteExport`）。输出目录结构：

```
backup/
├── articles/
│   ├── <slug>.html            # 成品 HTML，图片/媒体引用（img/source/video/audio 的
│   │                          # src、img/source 的 srcset、video 的 poster）已重写为
│   │                          # /images/<key>.<ext>，ActionText 的
│   │                          # <action-text-attachment> 外壳标签已剥离
│   └── <slug>.source.html     # 原始正文存档（未经任何修改）
├── pages/
│   ├── <slug>.html
│   └── <slug>.source.html
├── images/
│   └── <key>.<ext>            # 全部图片原文件，扩展名按 content_type 推导，
│                              # ActiveStorage blob 均已通过 base64 MD5 checksum 校验；
│                              # 收编的远程图片命名为 remote-<md5(url)>.<ext>
└── data/
    ├── articles.jsonl
    ├── pages.jsonl
    ├── tags.jsonl
    ├── article_tags.jsonl
    ├── comments.jsonl
    ├── redirects.jsonl
    ├── subscribers.jsonl
    ├── blobs.jsonl
    └── url_map.jsonl
```

### articles.jsonl（每行一个 JSON 对象）

```json
{
  "id": 123,
  "slug": "hello-world",
  "title": "...",
  "status": "publish",
  "created_at": "2024-03-15T08:00:00Z",
  "updated_at": "...",
  "scheduled_at": null,
  "description": "...",
  "excerpt": "...",
  "meta_title": "...", "meta_description": "...", "meta_image": "...",
  "source_url": "...", "source_author": "...",
  "comment": true,
  "content_file": "articles/hello-world.html",
  "source_file": "articles/hello-world.source.html",
  "source_content_type": "rich_text"
}
```

### pages.jsonl

字段同上，减去 SEO/source 字段，增加 `page_order`（整数）和 `redirect_url`（字符串，可空）。

### tags.jsonl / article_tags.jsonl

- tags：`{"id", "name", "slug"}`
- article_tags：`{"article_slug", "tag_slug"}`（导出侧已解析为 slug，免 ID 映射）

### comments.jsonl

```json
{
  "id": 1,
  "commentable_type": "Article",
  "commentable_slug": "hello-world",
  "parent_id": null,
  "author_name": "...", "author_email": "...", "author_url": "...",
  "author_username": "...", "author_avatar_url": "...",
  "content": "...",
  "status": "approved",
  "platform": null,
  "external_id": null,
  "url": null,
  "published_at": null,
  "created_at": "..."
}
```

`platform`/`external_id` 是从社交媒体回捞的评论标识（可能为 null）；`parent_id` 用于楼中楼回复。

### redirects.jsonl

`{"regex", "replacement", "enabled", "permanent"}` —— 原系统支持正则重定向，Go 侧需实现等价匹配逻辑。

### subscribers.jsonl

`{"email", "confirmed_at", "unsubscribed_at", "confirmation_token", "unsubscribe_token", "created_at", "tag_slugs": [...]}`。token 必须原样保留，否则已发出邮件里的退订/确认链接会失效。

### blobs.jsonl

```json
{
  "key": "abc123...",
  "filename": "photo.jpg",
  "content_type": "image/jpeg",
  "byte_size": 12345,
  "checksum_base64_md5": "...",
  "file": "images/abc123....jpg"
}
```

收编的远程图片多一个 `remote_url` 字段（原始 URL），其 `key` 为 `remote-<md5(url)>`：

```json
{
  "key": "remote-9e107d9d372bb6826bd81d3542a419d6",
  "filename": "cover.png",
  "content_type": "image/png",
  "byte_size": 6789,
  "checksum_base64_md5": "...",
  "file": "images/remote-9e107d9d372bb6826bd81d3542a419d6.png",
  "remote_url": "https://example.com/cover.png"
}
```

### url_map.jsonl

`{"old_path": "/rails/active_storage/blobs/redirect/<signed_id>/<filename>", "new_path": "/images/<key>.<ext>"}`

signed_id 是永久有效的签名 ID，已发出的 newsletter 邮件和搜索引擎收录页面里都嵌着这种绝对 URL。

## 4. Go 侧实现任务

1. **导入器**（建议做成独立命令，如 `cmd/migrate`）：
   - 读取迁移包目录，按 slug 幂等 upsert（可重复执行，用于全量→增量重跑）。
   - 枚举字符串按 §1.2 映射回 Go 侧表示。
   - 正文读 `content_file` 入库（`content_type=html`）；`source_file` 内容原样存入存档字段/表。
   - 导入 tags（按 slug 去重）、article_tags、comments（含 parent_id 楼中楼、platform 外部评论）、pages、redirects、subscribers、blobs 登记表。
   - 时间戳按 UTC ISO8601 解析。
2. **媒体服务**：`GET /images/<key>.<ext>` 从本地目录（或对象存储）直出，按扩展名/content_type 设置正确 `Content-Type`，加 `Cache-Control: public, max-age=31536000, immutable`。
3. **旧 URL 兼容路由**：`GET /rails/active_storage/*` → 查 `url_map` → **301** 到新路径。查不到返回 404 并记警告日志。
4. **重定向规则**：实现 redirects 的正则匹配（`regex` → `replacement`，`permanent` 决定 301/302，`enabled` 开关），注意匹配优先级和性能。
5. **渲染**：`html` 类型文章直接输出存储的 HTML（自行评估是否需要在输出侧做 sanitize）；`markdown` 类型用 goldmark 渲染，仅用于新内容。

## 5. 验收标准（全部通过才算迁移完成）

- [ ] blobs.jsonl 记录数 = `images/` 目录文件数 = 源库非 variant 的 `active_storage_blobs` 行数 + 收编的远程图片数（`remote_url` 字段可区分）
- [ ] 每个图片文件重新计算 MD5，与 `checksum_base64_md5` 一致
- [ ] 文章/页面/评论/标签/订阅者计数与源库一致
- [ ] 每篇文章成品 HTML 中的 `<img>` 数 = 该文章的附件数（防止 URL 重写漏网）
- [ ] 全部成品 HTML 中不再出现 `/rails/active_storage/` 或 `action-text-attachment` 字样（应已被重写）
- [ ] 抽查 10 篇文章人工对比渲染效果，必须包含：带 `<figure>/<figcaption>` 的、带表格的、带代码块的、含外部图片的
- [ ] 旧图片 URL 访问返回 301 且 Location 可访问
- [ ] 导入器重复执行两次，结果一致（幂等性）

## 6. 明确的非目标

- 不迁移：ActiveStorage variants（缩略图缓存）、sessions、activity_logs、social_media_posts、crossposts、twitter_archive_*（如后续需要再议）
- 不在迁移中做任何内容格式转换（HTML→Markdown 等）
- 不改动 S3 上的原有对象（导出是只读 + 下载）
