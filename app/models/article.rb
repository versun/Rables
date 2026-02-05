class Article < ApplicationRecord
  # Virtual attributes for crosspost functionality
  attr_accessor :crosspost_mastodon, :crosspost_twitter, :crosspost_bluesky, :crosspost_xiaohongshu
  # Virtual attributes for newsletter functionality
  attr_accessor :send_newsletter, :resend_newsletter

  has_rich_text :content
  has_many :social_media_posts, dependent: :destroy
  has_many :comments, as: :commentable, dependent: :destroy
  has_many :article_tags, dependent: :destroy
  has_many :tags, through: :article_tags
  accepts_nested_attributes_for :social_media_posts, allow_destroy: true

  enum :status, [ :draft, :publish, :schedule, :trash, :shared ]
  enum :content_type, { rich_text: "rich_text", html: "html" }, default: "rich_text"

  EXCERPT_LENGTH = 200

  before_validation :generate_slug
  before_validation :sync_excerpt
  validates :slug, presence: true, uniqueness: true
  validates :scheduled_at, presence: true, if: :schedule?
  validates :html_content, presence: true, if: -> { html? }
  validate :rich_text_content_presence

  scope :published, -> { where(status: :publish) }
  scope :scheduled, -> { where(status: :schedule) }
  scope :by_status, ->(status) { where(status: status) }
  # scope :paginate, ->(page, per_page) { offset((page - 1) * per_page).limit(per_page) }
  scope :publishable, -> { where(status: :schedule).where("scheduled_at <= ?", Time.current) }

  before_save :handle_time_zone, if: -> { schedule? && scheduled_at_changed? }
  before_save :cleanup_empty_social_media_posts
  after_save :schedule_publication, if: :should_schedule?
  after_save :handle_crosspost, if: -> { Setting.table_exists? }
  after_save :handle_newsletter, if: -> { Setting.table_exists? }

  # SQLite原生搜索功能
  scope :search_content, ->(query) {
    return all if query.blank?

    # 简单的LIKE搜索，适用于SQLite
    search_term = "%#{query}%"

    # 搜索标题、slug、描述和内容
    where(
      "articles.title LIKE :term OR
       articles.slug LIKE :term OR
       articles.description LIKE :term OR
       articles.id IN (SELECT record_id FROM action_text_rich_texts
              WHERE record_type = 'Article' AND name = 'content' AND body LIKE :term)",
      term: search_term
    )
  }



  def to_param
    slug
  end

  def publish_scheduled
    update(status: :publish, scheduled_at: nil, created_at: Time.current) if should_publish?
  end

  # 提取文章内容中的第一张图片，用于crosspost
  def first_image_attachment
    all_image_attachments.first
  end

  # 提取文章内容中的所有图片，用于crosspost
  # 各平台限制：Twitter/X、Mastodon、Bluesky 都最多支持4张图片
  def all_image_attachments(limit = 4)
    images = []

    # 首先尝试从 ActionText attachables 中查找（Trix 编辑器上传的图片）
    if content.present? && content.body.respond_to?(:attachables)
      content.body.attachables.each do |a|
        break if images.size >= limit

        if a.is_a?(ActiveStorage::Blob) && a.content_type&.start_with?("image/")
          images << a
        elsif a.class.name == "ActionText::Attachables::RemoteImage"
          url = a.try(:url)
          images << a if url.present? && url.is_a?(String)
        end
      end
    end

    # 对于 TinyMCE 编辑器，从 HTML 中提取所有图片 URL
    if images.size < limit
      existing_blob_ids = images.filter_map { |img| img.is_a?(ActiveStorage::Blob) ? img.id : nil }
      html = html? ? html_content : content.to_s
      if html.present?
        doc = Nokogiri::HTML5.fragment(html)
        doc.css("img").each do |img|
          break if images.size >= limit

          src = img["src"].to_s.strip
          next if src.blank?

          # 如果是 ActiveStorage 的 blob URL，尝试找到对应的 Blob
          blob = find_blob_from_url(src)
          if blob
            next if existing_blob_ids.include?(blob.id)

            images << blob
            existing_blob_ids << blob.id
          else
            images << RemoteImageWrapper.new(src)
          end
        end
      end
    end

    images
  end

  # 从 ActiveStorage URL 中查找 Blob
  def find_blob_from_url(url)
    return nil if url.blank?

    # 匹配 /rails/active_storage/blobs/redirect/:signed_id/:filename
    # 或 /rails/active_storage/blobs/proxy/:signed_id/:filename
    # 或 /rails/active_storage/blobs/:signed_id/:filename (旧格式)
    # signed_id 格式: message--signature (URL-safe base64 包含 A-Z, a-z, 0-9, -, _, = 和 -- 分隔符)
    if url =~ /\/rails\/active_storage\/blobs\/[^\/]+\/([A-Za-z0-9_-]+(?:={0,2})--[A-Za-z0-9_-]+)/
      signed_id = $1
      begin
        blob = ActiveStorage::Blob.find_signed(signed_id)
        return blob if blob&.content_type&.start_with?("image/")
      rescue => e
        Rails.logger.warn "Failed to find blob from signed_id: #{signed_id}, error: #{e.message}"
      end
    end

    # 匹配 /rails/active_storage/representations/.../:signed_blob_id/...
    if url =~ /\/rails\/active_storage\/representations\/[^\/]+\/([^\/]+)/
      signed_id = $1
      begin
        blob = ActiveStorage::Blob.find_signed(signed_id)
        return blob if blob&.content_type&.start_with?("image/")
      rescue => e
        Rails.logger.warn "Failed to find blob from signed_id: #{signed_id}, error: #{e.message}"
      end

      if signed_id =~ /\A\d+\z/
        begin
          blob = ActiveStorage::Blob.find_by(id: signed_id)
          return blob if blob&.content_type&.start_with?("image/")
        rescue => e
          Rails.logger.warn "Failed to find blob from blob_id: #{signed_id}, error: #{e.message}"
        end
      end
    end

    nil
  end

  # Virtual attribute for tag list (comma-separated tags)
  def tag_list
    tags.map(&:name).join(", ")
  end

  def tag_list=(names)
    self.tags = Tag.find_or_create_by_names(names)
  end

  # 根据content_type返回相应的内容
  def rendered_content
    cache_key = "#{cache_key_with_version}/rendered_content"

    Rails.cache.fetch(cache_key, expires_in: 7.days) do
      raw_html = if html?
        sanitize_html(html_content)
      else
        content.to_s
      end

      # Add loading="lazy" to all images
      add_lazy_loading_to_images(raw_html.to_s)
    end
  end

  # Sanitize HTML content to remove dangerous tags while preserving allowed tags
  def sanitize_html(html)
    return "" if html.blank?
    sanitizer = Rails::Html::SafeListSanitizer.new
    # Use allowed tags similar to ApplicationHelper#allowed_html_tags
    allowed_tags = %w[p br div span h1 h2 h3 h4 h5 h6 a img ul ol li dl dt dd table thead tbody tfoot tr th td caption colgroup col strong b em i u s strike del ins mark small blockquote q cite pre code kbd samp var hr figure figcaption article section aside header footer nav main details summary abbr address time sub sup ruby rt rp iframe video audio source]
    allowed_attributes = %w[href src alt title class id style target rel width height colspan rowspan loading controls autoplay loop muted frameborder allow allowfullscreen]
    sanitizer.sanitize(html, tags: allowed_tags, attributes: allowed_attributes)
  end

  # 获取内容的纯文本版本（用于社交媒体等）
  def plain_text_content
    if html?
      ActionView::Base.full_sanitizer.sanitize(html_content || "")
    else
      content.present? ? content.to_plain_text : ""
    end
  end

  # 检查是否有引用信息
  def has_source?
    source_url.present?
  end

  # SEO Meta 字段的默认值方法
  def seo_meta_title
    meta_title.presence || title
  end

  def seo_meta_description
    if meta_description.present?
      meta_description
    else
      # 使用文章开头的纯文本，截取前160个字符
      text = plain_text_content
      if text.present?
        # 移除多余的空白字符
        text = text.squish
        # 截取前160个字符，如果超过则在单词边界截断
        if text.length > 160
          text = text[0..156] + "..."
        end
        text
      else
        description
      end
    end
  end

  def seo_meta_image
    if meta_image.present?
      meta_image
    else
      # 尝试从文章内容中获取第一张图片
      image_attachment = first_image_attachment
      if image_attachment
        if image_attachment.is_a?(ActiveStorage::Blob)
          # 公共对象直接返回服务 URL，私有对象返回相对路径
          if image_attachment.service.public?
            image_attachment.url
          else
            Rails.application.routes.url_helpers.rails_blob_path(image_attachment, only_path: true)
          end
        elsif image_attachment.class.name == "ActionText::Attachables::RemoteImage"
          image_attachment.try(:url)
        end
      else
        nil
      end
    end
  end

  private

  def generate_slug
    if slug.blank?
      if title.present?
        self.slug = title.parameterize
      else
        # Generate slug from timestamp without setting title
        self.slug = DateTime.current.strftime("%Y-%m-%d-%H-%M").parameterize
      end
    end

    # Remove dots from slug if present
    self.slug = slug.gsub(".", "") if slug.include?(".")
  end

  def should_publish?
    schedule? && scheduled_at <= Time.current
  end

  def should_schedule?
    schedule? # && scheduled_at_changed?
  end

  def schedule_publication
    Rails.event.notify("article.publication_scheduled", level: "info", component: "Article", article_id: id, scheduled_at: scheduled_at)
    PublishScheduledArticlesJob.schedule_at(self)
  end

  def handle_crosspost
    return false unless publish?

    %w[mastodon twitter bluesky xiaohongshu].each do |platform|
      should_post = should_crosspost_to?(platform)
      Rails.event.notify("article.crosspost_check", level: "info", component: "Article", article_id: id, platform: platform, should_post: should_post)
      if should_post
        if platform == "xiaohongshu"
          Rails.event.notify("article.crosspost_skipped", level: "info", component: "Article", article_id: id, platform: platform, reason: "no_public_api")
          next
        end
        CrosspostArticleJob.perform_later(id, platform)
      end
    end
  end

  def should_send_newsletter?
    Rails.event.notify("article.newsletter_check_started", level: "info", component: "Article", article_id: id, status: status, publish: publish?, send_newsletter: send_newsletter.inspect, resend_newsletter: resend_newsletter.inspect)

    unless publish?
      Rails.event.notify("article.newsletter_check_skipped", level: "info", component: "Article", article_id: id, reason: "not_published")
      return false
    end

    newsletter_setting = NewsletterSetting.instance
    enabled = newsletter_setting.enabled?
    configured = newsletter_setting.configured?
    missing_fields = newsletter_setting.missing_config_fields

    Rails.event.notify("article.newsletter_setting_checked", level: "info", component: "Article", article_id: id, enabled: enabled, configured: configured, provider: newsletter_setting.provider)
    if missing_fields.any?
      Rails.event.notify("article.newsletter_missing_fields", level: "warn", component: "Article", article_id: id, missing_fields: missing_fields.join(", "))
    end

    unless enabled && configured
      Rails.event.notify("article.newsletter_check_skipped", level: "info", component: "Article", article_id: id, reason: "not_enabled_or_configured")
      return false
    end

    # 检查 send_newsletter 虚拟属性（用于新文章）
    # Rails check_box 会发送 "1" 当勾选时，"0" 当未勾选时
    # 但也可能收到 true/false 布尔值，或者字符串 "true"/"false"
    send_checked = send_newsletter.to_s == "1" || send_newsletter == true || send_newsletter.to_s == "true"

    # 检查 resend_newsletter 虚拟属性（用于已存在文章）
    resend_checked = resend_newsletter.to_s == "1" || resend_newsletter == true || resend_newsletter.to_s == "true"

    # 只要勾选了任一复选框即发送
    result = send_checked || resend_checked

    Rails.event.notify("article.newsletter_check_result", level: "info", component: "Article", article_id: id, should_send: result, send_checked: send_checked, resend_checked: resend_checked)

    result
  end

  def handle_newsletter
    Rails.event.notify("article.handle_newsletter_called", level: "info", component: "Article", article_id: id, status: status)

    unless should_send_newsletter?
      Rails.event.notify("article.newsletter_send_skipped", level: "info", component: "Article", article_id: id, reason: "should_send_newsletter_false")
      return
    end

    newsletter_setting = NewsletterSetting.instance
    Rails.event.notify("article.newsletter_sending", level: "info", component: "Article", article_id: id, provider: newsletter_setting.provider)

    if newsletter_setting.native?
      Rails.event.notify("article.newsletter_job_enqueued", level: "info", component: "Article", article_id: id, job: "NativeNewsletterSenderJob")
      NativeNewsletterSenderJob.perform_later(id)
    elsif newsletter_setting.listmonk?
      Rails.event.notify("article.newsletter_job_enqueued", level: "info", component: "Article", article_id: id, job: "ListmonkSenderJob")
      ListmonkSenderJob.perform_later(id)
    else
      Rails.event.notify("article.unknown_newsletter_provider", level: "warn", component: "Article", article_id: id, provider: newsletter_setting.provider)
    end
  end

  def handle_time_zone
    # Make sure scheduled_at is interpreted correctly
    # This ensures Rails knows this time is already in the application time zone
    self.scheduled_at = scheduled_at.in_time_zone(CacheableSettings.site_info[:time_zone]).utc if scheduled_at.present?
  end

  def should_crosspost_to?(platform)
    # 首先检查平台是否启用（后端安全检查）
    platform_enabled = Crosspost.find_by(platform: platform)&.enabled?
    return false unless platform_enabled

    # 检查 crosspost
    crosspost_checked = send("crosspost_#{platform}") == "1"

    # 只要勾选了任一复选框即发布
    crosspost_checked
  end

  def cleanup_empty_social_media_posts
    social_media_posts.each do |post|
      post.mark_for_destruction if post.url.blank?
    end
  end

  def rich_text_content_presence
    if rich_text?
      text = content.present? ? content.to_plain_text.to_s.strip : ""
      if text.blank?
        errors.add(:content, "can't be blank")
      end
    end
  end

  def sync_excerpt
    self.excerpt = build_excerpt
  end

  def build_excerpt
    source = description.presence || plain_text_content
    text = source.to_s.squish
    return nil if text.blank?

    text.truncate(EXCERPT_LENGTH, separator: /\s/)
  end

  def add_lazy_loading_to_images(html)
    return html if html.blank?

    doc = Nokogiri::HTML5.fragment(html)
    doc.css("img").each do |img|
      img.set_attribute("loading", "lazy") unless img["loading"].present?
    end
    doc.to_html.html_safe
  end
end

# Wrapper class for remote image URLs extracted from HTML content
# This class provides a compatible interface with ActionText::Attachables::RemoteImage
# so that social media services can handle both types uniformly
class RemoteImageWrapper
  attr_reader :url

  def initialize(url)
    @url = url
  end

  # Allow accessing url via try method (compatible with ActionText::Attachables::RemoteImage)
  def try(method_name)
    method_name == :url ? @url : nil
  end

  # Return class name as string for type checking (matches ActionText::Attachables::RemoteImage)
  def self.name
    "ActionText::Attachables::RemoteImage"
  end
end
