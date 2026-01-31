# Rables → Jekyll CMS 改造实施计划

## 项目概述

将 Rables 从完整博客系统改造为纯 CMS 后台系统，作为 Jekyll 静态站点的内容管理工具。

### 重要修正与决策项（必须先确认）
- **后台登录与初始化必须保留**：`SessionsController`、`UsersController`、`SetupController` 不能删除，否则后台无法登录/初始化（当前路由仍依赖它们）。
- **后台依赖的 Stimulus 控制器不能删除**：`sidebar_controller`、`fetch_comments_controller` 被后台视图使用，需保留。
- **若删除 `application.html.erb`**：必须为登录/注册/初始化页面指定 `layout "admin"` 或新增一个轻量布局，否则默认布局缺失会报错。
- **若删除 `application.css`**：后台布局仍引用它，需改为只加载 `admin.css` 或保留一个最小占位文件。
- **公共入口已确认保留**：订阅/评论/静态文件公共访问保留，相关控制器/路由/视图/MathCaptcha 依赖需保留。
- **密码找回已确认移除**：`passwords` 路由与相关视图/控制器可以删除。

### 改造目标
- 移除所有前台展示功能
- 保留完整的 Admin 管理功能（文章管理、社交跨发、邮件通讯等）
- 新增 Jekyll 文件管理和同步功能
- 保持 TinyMCE 编辑器不变

### 预计工作量
- 总计：8-10 个工作日
- 阶段一（前台剥离）：1-2 天
- 阶段二（Jekyll 集成）：4-5 天
- 阶段三（测试完善）：2-3 天

---

## 阶段一：前台代码剥离（1-2 天）

### 1.1 删除前台控制器

**需要删除的文件：**
```
app/controllers/articles_controller.rb
app/controllers/pages_controller.rb
app/controllers/tags_controller.rb
app/controllers/sitemap_controller.rb
app/controllers/passwords_controller.rb
```

**必须保留（公共入口已确认保留）：**
```
app/controllers/subscriptions_controller.rb
app/controllers/comments_controller.rb
app/controllers/static_files_controller.rb
```

**必须保留的文件（后台登录/初始化所需）：**
```
app/controllers/sessions_controller.rb
app/controllers/users_controller.rb
app/controllers/setup_controller.rb
```

### 1.2 删除前台视图

**需要删除的目录：**
```
app/views/articles/          （整个目录）
app/views/pages/             （整个目录，保留 admin/pages）
app/views/tags/              （整个目录，保留 admin/tags）
app/views/sitemap/           （整个目录）
app/views/passwords/         （整个目录）
app/views/pwa/               （整个目录）
app/views/components/        （整个目录 - 前台导航栏、页脚）
```

**必须保留的目录（公共入口所需）：**
```
app/views/subscriptions/
app/views/comments/
```

**必须保留的目录（后台登录/初始化所需）：**
```
app/views/sessions/
app/views/users/
app/views/setup/
```

**需要删除的前台模板：**
```
app/views/articles/index.rss.builder
app/views/tags/show.rss.builder
app/views/sitemap/index.xml.builder
```

**需要删除的布局文件：**
```
app/views/layouts/application.html.erb  （前台主布局）
```

**保留的布局文件：**
```
app/views/layouts/admin.html.erb        （后台布局 - 保留）
app/views/layouts/mailer.html.erb       （邮件布局 - 保留）
app/views/layouts/mailer.text.erb       （邮件布局 - 保留）
```

**注意：** 若删除 `application.html.erb`，需确保 `SessionsController` / `UsersController` / `SetupController` 显式使用 `layout "admin"` 或创建专用的轻量布局。
**补充：** 订阅/评论公共页面若继续渲染 HTML，需要迁移到 `admin` 或新的轻量布局。

### 1.3 清理路由配置

**修改文件：** `config/routes.rb`

**需要删除的路由（约 30 行）：**
- 第 3 行：`root "articles#index"` → 改为指向 admin
- 第 8 行：`resources :passwords`（若不再提供前台找回密码）
- 第 125-126 行：RSS Feed 和 Sitemap 路由
- 第 129-138 行：前台页面和文章路由

**需要添加的路由：**
- `root to: redirect('/admin')` 或 `root "admin/articles#index"`

**保留建议：**
- `resource :session`, `resources :users`, `resource :setup` 建议保留（后台登录与初始化需要）
- 订阅/评论/静态文件公共访问已确认保留，对应路由需保留并评估 CSRF/CORS 与速率限制。

### 1.4 删除前台 JavaScript 控制器

**需要删除的文件：**
```
app/javascript/controllers/theme_toggle_controller.js
app/javascript/controllers/newsletter_subscription_controller.js
app/javascript/controllers/share_controller.js
app/javascript/controllers/password_toggle_controller.js
```

**保留的文件：**
```
app/javascript/controllers/batch_selection_controller.js  （后台批量选择）
app/javascript/controllers/newsletter_controller.js       （后台邮件设置）
app/javascript/controllers/source_reference_controller.js （后台源引用）
app/javascript/tinymce_config.js                          （TinyMCE 配置）
```

**修正：后台仍使用的控制器需要保留**
```
app/javascript/controllers/sidebar_controller.js
app/javascript/controllers/fetch_comments_controller.js
app/javascript/controllers/math_captcha_controller.js
```

### 1.5 删除前台样式和资源

**需要删除的文件：**
```
app/assets/stylesheets/application.css  （前台样式）
```

**保留的文件：**
```
app/assets/stylesheets/admin.css        （后台样式）
```

**注意：** 后台布局仍加载 `application.css`，删除前需同步修改 `app/views/layouts/admin.html.erb` 或保留最小文件。
**补充：** 订阅/评论公共页面也依赖部分前台样式，需保留必要样式或迁移到新样式文件。

### 1.6 删除前台 Helpers

**需要删除的文件：**
```
app/helpers/articles_helper.rb          （空文件）
app/helpers/pages_helper.rb
app/helpers/users_helper.rb
app/helpers/newsletters_helper.rb       （空文件）
```

**保留的文件：**
```
app/helpers/application_helper.rb       （通用方法）
app/helpers/admin_helper.rb             （后台方法）
app/helpers/settings_helper.rb          （设置方法）
app/helpers/git_integrations_helper.rb  （Git 集成）
app/helpers/analytics_helper.rb         （分析方法）
app/helpers/math_captcha_helper.rb      （验证码 - 公共订阅/评论需要）
```

### 1.7 清理 Controller Concerns

**需要评估的文件：**
```
app/controllers/concerns/authentication.rb       （评估是否仅前台使用）
app/controllers/concerns/math_captcha_verification.rb （前台评论/订阅用）
```
**修正：** `authentication.rb` 是后台登录必需，必须保留；`math_captcha_verification.rb` 因公共订阅/评论保留而必须保留。

### 1.8 修改 ApplicationController

**修改文件：** `app/controllers/application_controller.rb`

**需要移除的内容：**
- `helper_method :navbar_items` 方法声明
- `def navbar_items` 方法定义
- `def refresh_pages` 方法定义（如果仅前台使用）
- 评估 `include Authentication` 是否需要保留

**修正：** `include Authentication` 仍为后台登录必需，需保留。

### 1.9 更新 Stimulus 控制器索引

**修改文件：** `app/javascript/controllers/index.js`

**修正：** 当前使用 `eagerLoadControllersFrom`，无需手动移除 import/register；只需确保删除的 controller 文件不再存在。

### 1.10 验证阶段一完成

**验证步骤：**
1. 运行 `bin/rails routes` 确认前台路由已移除
2. 运行 `bin/rails test` 确认无测试失败
3. 启动服务器访问 `/admin` 确认后台功能正常
4. 测试以下 Admin 功能：
   - 文章 CRUD
   - 社交跨发配置和发布
   - 邮件通讯配置和发送
   - 评论管理
   - 订阅者管理
   - 设置页面
   - Git 集成配置

**补充验证：**
- 登录/退出流程正常（`/session`）
- 初始化流程正常（`/setup`）
- 订阅/评论/静态文件公共访问正常（`/subscriptions`、`/comments`、`/static/*`）
- 若删除前台测试，`test/controllers/*` 与 `test/system/*` 前台用例已同步清理

---

## 阶段二：Jekyll 集成开发（4-5 天）

### 2.0 数据映射与策略确认（新增）
- **Jekyll `date` 映射规则**：使用 `created_at` / `scheduled_at` / 新增字段的优先级与时区需明确。
- **文件命名与清理策略**：slug/日期变化时如何删除旧文件；是否以 `id` 作为内部索引。
- **permalink 规则**：需兼容 `article_route_prefix` 配置。
- **路径安全**：`jekyll_path` 必须校验为可写目录，防止路径穿越与误删。

### 2.1 创建 Jekyll 设置模型

**新建文件：** `app/models/jekyll_setting.rb`

**功能说明：**
- 存储 Jekyll 项目配置（单例模式）
- 字段包括：
  - `jekyll_path`: Jekyll 项目本地路径
  - `repository_type`: 仓库类型（local/git）
  - `repository_url`: Git 仓库地址（可选）
  - `branch`: Git 分支（默认 main）
  - `posts_directory`: 文章目录（默认 `_posts`）
  - `pages_directory`: 页面目录（默认 `_pages` 或根目录）
  - `assets_directory`: 资源目录（默认 `assets/images`）
  - `front_matter_mapping`: Front Matter 字段映射配置（JSON）
  - `auto_sync_enabled`: 是否启用自动同步
  - `sync_on_publish`: 发布时自动同步
  - `last_sync_at`: 最后同步时间

**建议校验：**
- `jekyll_path` 必须存在且可写
- `repository_url` 必须是合法 Git URL（当 `repository_type=git`）
- `front_matter_mapping` 必须是合法 JSON

**数据库迁移：** 创建 `jekyll_settings` 表

### 2.2 创建 Jekyll 导出模型

**新建文件：** `app/models/jekyll_export.rb`

**功能说明：**
- 基于现有 `MarkdownExport` 扩展
- 生成 Jekyll 兼容的目录结构和文件格式
- 核心方法：
  - `generate`: 生成完整导出
  - `export_article(article)`: 导出单篇文章
  - `export_page(page)`: 导出单个页面
  - `build_front_matter(item)`: 构建 Jekyll Front Matter
  - `process_content(content)`: 处理内容中的图片和链接
  - `copy_attachments(item)`: 复制附件到 assets 目录

**Jekyll Front Matter 格式：**
```yaml
---
layout: post
title: "文章标题"
date: 2024-01-01 10:00:00 +0800
categories: [tag1, tag2]
tags: [tag1, tag2]
description: "文章描述"
image: /assets/images/cover.jpg
author: 作者名
---
```

**目录结构：**
```
jekyll_project/
├── _posts/
│   └── 2024-01-01-article-slug.md
├── _pages/
│   └── about.md
└── assets/
    └── images/
        └── article-images/
```

**补充：**
- 导出时复用 `Exports::HtmlAttachmentProcessing` 处理 ActionText/TinyMCE 图片，避免重复逻辑。
- 明确 `layout` 字段来源（文章/页面/自定义映射）。

### 2.3 创建 Jekyll 同步服务

**新建文件：** `app/services/jekyll_sync_service.rb`

**功能说明：**
- 将导出内容同步到 Jekyll 项目
- 支持本地文件系统和 Git 仓库两种模式
- 核心方法：
  - `sync_all`: 同步所有内容
  - `sync_article(article)`: 同步单篇文章
  - `sync_page(page)`: 同步单个页面
  - `delete_article(article)`: 删除文章文件
  - `delete_page(page)`: 删除页面文件
  - `commit_changes(message)`: Git 提交更改
  - `push_to_remote`: 推送到远程仓库

**同步逻辑：**
1. 检查 Jekyll 项目路径有效性
2. 生成/更新 Markdown 文件
3. 复制/更新附件文件
4. 处理已删除的内容（清理旧文件）
5. 如果启用 Git，执行 commit 和 push

**补充：**
- 同步过程需统一记录 `ActivityLog` 与 `Rails.event.notify`，便于排查问题。
- 删除策略需基于“旧文件映射表”或 “按 id 生成文件名”保证可追踪。

### 2.4 创建 Jekyll 同步记录模型

**新建文件：** `app/models/jekyll_sync_record.rb`

**功能说明：**
- 记录每次同步操作的历史
- 字段包括：
  - `sync_type`: 同步类型（full/incremental/single）
  - `status`: 状态（pending/in_progress/completed/failed）
  - `articles_count`: 同步的文章数
  - `pages_count`: 同步的页面数
  - `error_message`: 错误信息
  - `started_at`: 开始时间
  - `completed_at`: 完成时间
  - `git_commit_sha`: Git 提交 SHA（如果使用 Git）

**数据库迁移：** 创建 `jekyll_sync_records` 表

### 2.5 创建 Jekyll 后台任务

**新建文件：** `app/jobs/jekyll_sync_job.rb`

**功能说明：**
- 异步执行 Jekyll 同步操作
- 支持全量同步和增量同步
- 记录活动日志
- 错误处理和重试机制

**新建文件：** `app/jobs/jekyll_single_sync_job.rb`

**功能说明：**
- 同步单篇文章或页面
- 在文章发布时自动触发（如果启用）

**补充：**
- 定时发布任务 `PublishScheduledArticlesJob` 也应触发单篇同步（若开启自动同步）。

### 2.6 创建 Admin 控制器

**新建文件：** `app/controllers/admin/jekyll_controller.rb`

**功能说明：**
- 管理 Jekyll 集成的所有操作
- Actions：
  - `show`: 显示 Jekyll 设置和同步状态
  - `update`: 更新 Jekyll 设置
  - `sync`: 触发全量同步
  - `sync_article`: 同步单篇文章
  - `verify`: 验证 Jekyll 项目配置
  - `preview`: 预览导出的 Markdown

**新建文件：** `app/controllers/admin/jekyll_sync_records_controller.rb`

**功能说明：**
- 查看同步历史记录
- Actions：
  - `index`: 同步记录列表

### 2.7 创建 Admin 视图

**新建目录和文件：**
```
app/views/admin/jekyll/
├── show.html.erb           （Jekyll 设置和控制面板）
├── _form.html.erb          （设置表单）
├── _sync_status.html.erb   （同步状态组件）
└── preview.html.erb        （Markdown 预览）

app/views/admin/jekyll_sync_records/
└── index.html.erb          （同步历史列表）
```

**设置页面功能：**
- Jekyll 项目路径配置
- Git 仓库配置（可选）
- Front Matter 字段映射
- 自动同步开关
- 手动同步按钮
- 同步状态显示
- 最近同步记录

### 2.8 更新路由配置

**修改文件：** `config/routes.rb`

**添加的路由：**
```ruby
namespace :admin do
  resource :jekyll, only: [:show, :update], controller: 'jekyll' do
    post :sync
    post :sync_article
    post :verify
    get :preview
  end
  resources :jekyll_sync_records, only: [:index]
end
```

### 2.9 更新 Admin 侧边栏导航

**修改文件：** `app/views/admin/shared/_sidebar.html.erb`

**添加内容：**
- 在适当位置添加 "Jekyll" 或 "同步" 菜单项
- 链接到 Jekyll 设置页面

### 2.10 集成到文章发布流程

**修改文件：** `app/controllers/admin/articles_controller.rb`

**修改内容：**
- 在 `publish` 和 `update` action 中
- 检查是否启用了自动同步
- 如果启用，触发 `JekyllSingleSyncJob`

**修改文件：** `app/models/article.rb`

**添加内容：**
- `after_save` 回调（可选）
- 或在控制器中显式调用

**补充：**
- 建议在控制器显式触发，避免对后台批量操作造成隐式副作用。

### 2.11 增强 Markdown 导出

**修改文件：** `app/models/markdown_export.rb`

**需要增强的功能：**
- 支持 Jekyll 特定的 Front Matter 格式
- 支持自定义 Front Matter 字段映射
- 支持文件名格式配置（`YYYY-MM-DD-slug.md`）
- 改进图片路径处理（相对于 Jekyll assets 目录）

### 2.12 利用现有 Git 集成

**修改文件：** `app/models/git_integration.rb`

**添加方法：**
- `clone_repository(path)`: 克隆仓库到指定路径
- `pull_latest(path)`: 拉取最新代码
- `commit_and_push(path, message)`: 提交并推送

**或新建文件：** `app/services/git_operations_service.rb`

**功能说明：**
- 封装 Git 命令行操作
- 使用现有 GitIntegration 的认证信息
- 方法包括：clone, pull, add, commit, push
- 错误处理和日志记录

**补充：**
- 需复用 `GitIntegration#build_authenticated_url` 生成认证 URL。
- 处理 git 命令缺失/失败/冲突场景并记录。

### 2.13 附件/图片处理模块

**现有基础设施（保留）：**
- ActiveStorage 配置（`config/storage.yml`）：支持本地存储和 S3
- 编辑器图片上传（`app/controllers/admin/editor_images_controller.rb`）
- 图片处理模块（`app/models/concerns/exports/html_attachment_processing.rb`）

**需要增强的功能：**

**修改文件：** `app/models/concerns/exports/html_attachment_processing.rb`

增强内容：
- 支持 Jekyll 资源目录结构（`assets/images/posts/{slug}/`）
- 图片路径转换为 Jekyll 相对路径格式
- 保留图片的 alt 文本和 caption
- 支持 TinyMCE 编辑器的图片格式

**新建文件：** `app/services/jekyll_attachment_processor.rb`

功能说明：
- `process_article_attachments(article)`: 处理文章中的所有附件
- `copy_to_jekyll_assets(attachment, target_dir)`: 复制附件到 Jekyll assets 目录
- `convert_image_paths(content, article_slug)`: 转换内容中的图片路径
- `extract_images_from_html(html)`: 从 HTML 中提取所有图片
- `download_remote_image(url)`: 下载远程图片到本地

**Jekyll 图片目录结构：**
```
jekyll-site/
└── assets/
    └── images/
        └── posts/
            └── {article-slug}/
                ├── image1.jpg
                ├── image2.png
                └── cover.jpg
```

**Markdown 中的图片格式：**
```markdown
![Alt text](/assets/images/posts/my-article/image1.jpg)
```

### 2.14 URL 重定向导出

**现有基础设施（保留）：**
- Redirect 模型（`app/models/redirect.rb`）
- 重定向中间件（`app/middleware/redirect_middleware.rb`）
- Admin 管理界面（`app/controllers/admin/redirects_controller.rb`）

**新建文件：** `app/services/jekyll_redirects_exporter.rb`

功能说明：
- `export_to_netlify`: 生成 Netlify `_redirects` 文件
- `export_to_vercel`: 生成 Vercel `vercel.json` 重定向配置
- `export_to_htaccess`: 生成 Apache `.htaccess` 文件
- `export_to_nginx`: 生成 Nginx 重定向配置
- `export_to_jekyll_plugin`: 生成 `jekyll-redirect-from` 插件格式

**导出格式示例：**

Netlify `_redirects`:
```
/old-post  /new-post  301
/blog/*    /articles/:splat  302
```

Jekyll Front Matter（使用 jekyll-redirect-from 插件）:
```yaml
redirect_from:
  - /old-url/
  - /another-old-url/
```

**Jekyll 设置中添加字段：**
- `redirect_export_format`: 重定向导出格式（netlify/vercel/htaccess/nginx/jekyll-plugin）

### 2.15 静态文件导出

**现有基础设施（保留）：**
- StaticFile 模型（`app/models/static_file.rb`）
- Admin 管理界面（`app/controllers/admin/static_files_controller.rb`）
- ActiveStorage 附件存储

**新建文件：** `app/services/jekyll_static_files_exporter.rb`

功能说明：
- `export_all`: 导出所有静态文件到 Jekyll 目录
- `export_file(static_file)`: 导出单个静态文件
- `build_directory_structure`: 构建目录结构
- `update_references_in_content`: 更新内容中的静态文件引用

**Jekyll 静态文件目录结构：**
```
jekyll-site/
├── assets/
│   ├── documents/
│   │   └── document.pdf
│   └── downloads/
│       └── file.zip
└── static/
    └── {original-path}/
        └── {filename}
```

**路径映射规则：**
- 原路径 `/static/images/logo.png` → Jekyll 路径 `/assets/images/logo.png`
- 原路径 `/static/docs/guide.pdf` → Jekyll 路径 `/assets/documents/guide.pdf`

**Jekyll 设置中添加字段：**
- `static_files_directory`: 静态文件导出目录（默认 `assets`）
- `preserve_original_paths`: 是否保留原始路径结构

### 2.16 评论数据导出

**现有基础设施（保留）：**
- Comment 模型（`app/models/comment.rb`）
- 社交媒体评论导入（`app/jobs/fetch_social_comments_job.rb`）
- Admin 评论管理（`app/controllers/admin/comments_controller.rb`）
- 评论通知邮件（`app/jobs/comment_reply_notification_job.rb`）

**新建文件：** `app/services/jekyll_comments_exporter.rb`

功能说明：
- `export_all`: 导出所有已批准评论
- `export_for_article(article)`: 导出单篇文章的评论
- `build_comment_tree(comments)`: 构建嵌套评论树结构
- `format_for_yaml`: 格式化为 YAML 数据文件
- `format_for_json`: 格式化为 JSON 数据文件

**Jekyll 评论数据结构：**
```
jekyll-site/
└── _data/
    └── comments/
        ├── my-first-post.yml
        ├── another-post.yml
        └── ...
```

**评论 YAML 格式：**
```yaml
# _data/comments/my-post.yml
- id: 1
  type: local
  author:
    name: "John Doe"
    email_hash: "abc123..."  # MD5 hash for Gravatar
    url: "https://johndoe.com"
  content: "Great post!"
  date: 2024-01-15T10:30:00+08:00
  replies:
    - id: 2
      type: local
      author:
        name: "Admin"
      content: "Thanks for reading!"
      date: 2024-01-15T11:00:00+08:00

- id: 3
  type: mastodon
  author:
    name: "Jane Smith"
    username: "@jane@mastodon.social"
    avatar: "https://..."
  content: "Love this article!"
  date: 2024-01-15T12:00:00+08:00
  url: "https://mastodon.social/@jane/123456"
  platform: mastodon
```

**Jekyll 模板使用示例：**
```liquid
{% assign comments = site.data.comments[page.slug] %}
{% for comment in comments %}
  <div class="comment">
    <strong>{{ comment.author.name }}</strong>
    <p>{{ comment.content }}</p>
    {% if comment.replies %}
      {% for reply in comment.replies %}
        <div class="reply">...</div>
      {% endfor %}
    {% endif %}
  </div>
{% endfor %}
```

**Jekyll 设置中添加字段：**
- `export_comments`: 是否导出评论（默认 true）
- `comments_format`: 评论导出格式（yaml/json）
- `include_pending_comments`: 是否包含待审核评论（默认 false）
- `include_social_comments`: 是否包含社交媒体评论（默认 true）

**补充：**
- 默认仅导出已审核评论，避免公开未审核内容。

### 2.17 更新 Jekyll 设置模型

**修改文件：** `app/models/jekyll_setting.rb`

**添加字段（补充 2.1 节）：**
```
# 重定向相关
redirect_export_format: string (netlify/vercel/htaccess/nginx/jekyll-plugin)

# 静态文件相关
static_files_directory: string (默认 'assets')
preserve_original_paths: boolean (默认 false)

# 评论相关
export_comments: boolean (默认 true)
comments_format: string (yaml/json)
include_pending_comments: boolean (默认 false)
include_social_comments: boolean (默认 true)

# 图片相关
images_directory: string (默认 'assets/images/posts')
download_remote_images: boolean (默认 true)
```

---

## 阶段三：测试和完善（2-3 天）

### 3.1 编写单元测试

**新建测试文件：**
```
test/models/jekyll_setting_test.rb
test/models/jekyll_export_test.rb
test/models/jekyll_sync_record_test.rb
test/services/jekyll_sync_service_test.rb
test/services/jekyll_attachment_processor_test.rb
test/services/jekyll_redirects_exporter_test.rb
test/services/jekyll_static_files_exporter_test.rb
test/services/jekyll_comments_exporter_test.rb
test/jobs/jekyll_sync_job_test.rb
```

**测试覆盖：**
- Jekyll 设置的验证和默认值
- Markdown 导出格式正确性
- Front Matter 生成正确性
- 文件同步逻辑
- Git 操作（使用 mock）
- 错误处理
- 图片路径转换正确性
- 重定向导出各种格式
- 静态文件复制和路径映射
- 评论 YAML/JSON 格式正确性
- 嵌套评论结构

### 3.2 编写集成测试

**新建测试文件：**
```
test/controllers/admin/jekyll_controller_test.rb
test/controllers/admin/jekyll_sync_records_controller_test.rb
```

**测试覆盖：**
- 设置页面访问和更新
- 同步操作触发
- 权限验证

### 3.3 编写系统测试

**新建测试文件：**
```
test/system/admin/jekyll_test.rb
```

**测试覆盖：**
- 完整的 Jekyll 配置流程
- 手动同步操作
- 同步历史查看

### 3.4 验证现有功能

**验证清单：**
- [ ] 文章 CRUD 功能正常
- [ ] 批量操作（发布、删除、添加标签）正常
- [ ] 社交跨发功能正常
- [ ] 邮件通讯功能正常
- [ ] 评论管理功能正常
- [ ] 订阅者管理功能正常
- [ ] 设置页面功能正常
- [ ] 导出功能正常
- [ ] 导入功能正常
- [ ] Git 集成配置正常

### 3.5 文档更新

**修改文件：**
```
README.md              （更新项目说明）
AGENTS.md              （更新 AI 代理指南）
CLAUDE.md              （更新开发指南）
```

**文档内容：**
- 项目定位更新（CMS for Jekyll）
- 安装和配置说明
- Jekyll 集成使用指南
- 常见问题解答

### 3.6 数据库迁移

**创建迁移文件：**
```
db/migrate/xxx_create_jekyll_settings.rb
db/migrate/xxx_create_jekyll_sync_records.rb
```

**运行验证：**
```bash
bin/rails db:migrate
bin/rails db:rollback
bin/rails db:migrate
```

### 3.7 清理和优化

**清理任务：**
- 移除未使用的 Gem 依赖
- 清理未使用的测试文件
- 更新 Gemfile.lock
- 运行 `bin/rubocop` 修复代码风格

**补充清理：**
- 移除前台相关测试用例（保留订阅/评论/静态文件公共入口相关测试）。
- 清理前台专用 importmap 与 JS（例如 `highlight.js`），并同步调整 `app/javascript/application.js`。

**可选移除的 Gem：**
- `will_paginate`（如果仅前台使用）
- 其他仅前台使用的依赖

---

## 保留的完整功能清单

### Admin 控制器（17 个）
1. `ArticlesController` - 文章管理、批量操作、跨平台发布
2. `PagesController` - 页面管理
3. `TagsController` - 标签管理
4. `CommentsController` - 评论审核和管理
5. `SettingsController` - 网站设置
6. `SubscribersController` - 订阅者管理
7. `NewsletterController` - 邮件通讯配置
8. `CrosspostsController` - 社交平台配置
9. `GitIntegrationsController` - Git 平台配置
10. `MigratesController` - 数据导入导出
11. `DownloadsController` - 文件下载
12. `RedirectsController` - URL 重定向
13. `StaticFilesController` - 静态文件管理
14. `ActivitiesController` - 活动日志
15. `EditorImagesController` - 编辑器图片上传
16. `SourcesController` - Twitter 内容获取
17. `BaseController` - 基础控制器

### 后台任务（16 个）
1. `CrosspostArticleJob` - 社交媒体跨发
2. `FetchSocialCommentsJob` - 获取社交评论
3. `ScheduledFetchSocialCommentsJob` - 定时获取评论
4. `NativeNewsletterSenderJob` - 原生邮件发送
5. `ListmonkSenderJob` - Listmonk 邮件发送
6. `NewsletterConfirmationJob` - 订阅确认邮件
7. `CommentReplyNotificationJob` - 评论回复通知
8. `PublishScheduledArticlesJob` - 定时发布文章
9. `ExportDataJob` - 数据导出
10. `ExportMarkdownJob` - Markdown 导出
11. `ImportFromRssJob` - RSS 导入
12. `ImportFromZipJob` - ZIP 导入
13. `CleanOldExportsJob` - 清理旧导出
14. `PasswordResetJob` - 密码重置
15. `JekyllSyncJob` - Jekyll 全量同步（新增）
16. `JekyllSingleSyncJob` - Jekyll 单项同步（新增）

### 服务类（9 个）
1. `TwitterService` - Twitter/X API
2. `MastodonService` - Mastodon API
3. `BlueskyService` - Bluesky API
4. `ContentBuilder` - 社交内容构建
5. `JekyllSyncService` - Jekyll 同步（新增）
6. `JekyllAttachmentProcessor` - Jekyll 附件处理（新增）
7. `JekyllRedirectsExporter` - 重定向导出（新增）
8. `JekyllStaticFilesExporter` - 静态文件导出（新增）
9. `JekyllCommentsExporter` - 评论导出（新增）

### 核心模型
- Article, Page, Tag, Comment
- Setting, NewsletterSetting, Crosspost
- Subscriber, ActivityLog, SocialMediaPost
- Redirect, StaticFile, GitIntegration
- JekyllSetting（新增）
- JekyllSyncRecord（新增）

---

## 关键文件清单

### 需要删除的文件（约 60 个）
详见阶段一各小节

### 需要修改的文件（约 10 个）
- `config/routes.rb`
- `app/controllers/application_controller.rb`
- `app/controllers/admin/articles_controller.rb`
- `app/views/admin/shared/_sidebar.html.erb`
- `app/views/layouts/admin.html.erb`
- `app/models/markdown_export.rb`
- `app/javascript/controllers/index.js`
- `app/javascript/application.js`
- `config/importmap.rb`
- `README.md`
- `AGENTS.md`

### 需要新建的文件（约 20 个）
- `app/models/jekyll_setting.rb`
- `app/models/jekyll_export.rb`
- `app/models/jekyll_sync_record.rb`
- `app/services/jekyll_sync_service.rb`
- `app/services/jekyll_attachment_processor.rb`
- `app/services/jekyll_redirects_exporter.rb`
- `app/services/jekyll_static_files_exporter.rb`
- `app/services/jekyll_comments_exporter.rb`
- `app/services/git_operations_service.rb`（可选）
- `app/jobs/jekyll_sync_job.rb`
- `app/jobs/jekyll_single_sync_job.rb`
- `app/controllers/admin/jekyll_controller.rb`
- `app/controllers/admin/jekyll_sync_records_controller.rb`
- `app/views/admin/jekyll/show.html.erb`
- `app/views/admin/jekyll/_form.html.erb`
- `app/views/admin/jekyll_sync_records/index.html.erb`
- `db/migrate/xxx_create_jekyll_settings.rb`
- `db/migrate/xxx_create_jekyll_sync_records.rb`
- 相关测试文件

---

## 验证清单

### 阶段一完成验证
- [ ] 访问根路径自动跳转到 /admin
- [ ] 所有前台路由返回 404 或重定向
- [ ] Admin 所有功能正常工作
- [ ] 无 JavaScript 控制台错误
- [ ] 测试套件全部通过

### 阶段二完成验证
- [ ] Jekyll 设置页面可访问和配置
- [ ] 手动同步功能正常
- [ ] 单篇文章同步功能正常
- [ ] 同步历史记录正确
- [ ] Git 集成（如配置）正常工作
- [ ] 生成的 Markdown 格式正确
- [ ] Front Matter 包含所有必要字段
- [ ] 图片正确导出到 Jekyll assets 目录
- [ ] 图片路径在 Markdown 中正确转换
- [ ] 重定向规则正确导出（根据配置格式）
- [ ] 静态文件正确复制到 Jekyll 目录
- [ ] 评论数据正确导出为 YAML/JSON
- [ ] 嵌套评论结构正确保留
- [ ] 社交媒体评论正确标记来源

### 阶段三完成验证
- [ ] 测试覆盖率 >= 80%
- [ ] 无 Rubocop 错误
- [ ] 文档已更新
- [ ] 所有功能端到端测试通过

---

## 特殊功能模块处理说明

### 附件/图片处理

**现有能力：**
- ActiveStorage 存储（本地/S3）
- TinyMCE 编辑器图片上传
- `HtmlAttachmentProcessing` 模块处理导出时的图片

**Jekyll 改造：**
- 图片复制到 `assets/images/posts/{slug}/` 目录
- HTML 中的图片路径转换为 Markdown 格式
- 支持下载远程图片到本地
- 保留 alt 文本和 caption

### URL 重定向

**现有能力：**
- Redirect 模型（正则表达式支持）
- 中间件自动处理重定向
- Admin 管理界面

**Jekyll 改造：**
- 导出为静态配置文件（Netlify/Vercel/Apache/Nginx）
- 或使用 `jekyll-redirect-from` 插件格式
- 保留后台管理功能用于配置

### 静态文件

**现有能力：**
- StaticFile 模型管理
- ActiveStorage 存储
- `/static/*` 路径访问

**Jekyll 改造：**
- 复制文件到 Jekyll `assets/` 目录
- 更新内容中的引用路径
- 保留后台管理功能

### 评论系统

**现有能力：**
- Comment 模型（本地+社交媒体）
- 嵌套回复支持
- 社交媒体评论导入
- 评论通知邮件

**Jekyll 改造：**
- 导出已批准评论为 YAML/JSON 数据文件
- 保留嵌套结构
- 标记社交媒体来源
- 后台继续管理评论（审核、回复）
- 可选：保留 API 端点接收新评论

---

## 风险和注意事项

### 低风险
- 前台代码删除（模块边界清晰）
- 路由配置修改（影响范围有限）

### 中等风险
- Jekyll 文件格式兼容性（需测试多种 Jekyll 主题）
- Git 操作错误处理（网络、权限问题）

### 需要注意
- 删除前台代码前先创建 Git 分支
- 每个阶段完成后进行完整测试
- 保留数据库中的所有数据
- 邮件通讯功能依赖正确的 SMTP 配置

---

## 实施顺序建议

1. 创建新的 Git 分支 `feature/jekyll-cms`
2. 完成阶段一所有步骤
3. 运行测试，确认无回归
4. 完成阶段二数据库迁移
5. 完成阶段二模型和服务
6. 完成阶段二控制器和视图
7. 完成阶段二集成
8. 完成阶段三测试
9. 完成阶段三文档
10. 合并到主分支
