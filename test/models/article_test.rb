# frozen_string_literal: true

require "test_helper"

class ArticleTest < ActiveSupport::TestCase
  def setup
    @article = Article.new(
      title: "Test Article",
      description: "Test description",
      status: :draft,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )
  end

  test "should be valid with valid attributes" do
    assert @article.valid?
  end

  test "should generate slug from title" do
    @article.title = "My Test Article"
    @article.valid?
    assert_equal "my-test-article", @article.slug
  end

  test "should generate unique slug when duplicate exists" do
    Article.create!(title: "Test Article", status: :draft, content_type: :html, html_content: "<p>Content</p>")
    @article.title = "Test Article"
    @article.valid?
    # Slug will be different because we have two articles with same title
    assert @article.slug.present?
  end

  test "should remove dots from slug" do
    @article.title = "Article v1.0"
    @article.valid?
    assert_not @article.slug.include?(".")
  end

  test "should require slug to be unique" do
    existing_article = articles(:published_article)
    @article.slug = existing_article.slug
    assert_not @article.valid?
    assert_includes @article.errors[:slug], "has already been taken"
  end

  test "should require scheduled_at when status is schedule" do
    @article.status = :schedule
    @article.scheduled_at = nil
    assert_not @article.valid?
    assert_includes @article.errors[:scheduled_at], "can't be blank"
  end

  test "should not require scheduled_at when status is not schedule" do
    @article.status = :draft
    @article.scheduled_at = nil
    assert @article.valid?
  end

  test "published scope should return only published articles" do
    published = articles(:published_article)
    draft = articles(:draft_article)

    published_articles = Article.published
    assert_includes published_articles, published
    assert_not_includes published_articles, draft
  end

  test "by_status scope should filter by status" do
    draft_articles = Article.by_status(:draft)
    assert_includes draft_articles, articles(:draft_article)
    assert_not_includes draft_articles, articles(:published_article)
  end

  test "publishable scope should return scheduled articles ready to publish" do
    scheduled = articles(:scheduled_article)
    scheduled.update!(scheduled_at: 1.hour.ago)

    publishable = Article.publishable
    assert_includes publishable, scheduled
  end

  test "publishable scope should not return future scheduled articles" do
    scheduled = articles(:scheduled_article)
    scheduled.update!(scheduled_at: 1.hour.from_now)

    publishable = Article.publishable
    assert_not_includes publishable, scheduled
  end

  test "to_param should return slug" do
    @article.slug = "test-slug"
    assert_equal "test-slug", @article.to_param
  end

  test "publish_scheduled should update status to publish" do
    article = articles(:scheduled_article)
    article.update!(scheduled_at: 1.hour.ago)

    article.publish_scheduled
    assert article.publish?
    assert_nil article.scheduled_at
  end

  test "publish_scheduled should not update if not ready" do
    article = articles(:scheduled_article)
    article.update!(scheduled_at: 1.hour.from_now)
    original_status = article.status

    article.publish_scheduled
    assert_equal original_status, article.status
  end

  test "tag_list should return comma-separated tag names" do
    article = articles(:published_article)
    tag1 = tags(:ruby)
    tag2 = tags(:rails)
    article.tags << [ tag1, tag2 ]

    # Tags are returned in the order they were added
    tag_names = article.tag_list.split(", ")
    assert_includes tag_names, "Ruby"
    assert_includes tag_names, "Rails"
  end

  test "tag_list= should create tags from comma-separated string" do
    @article.save!
    @article.tag_list = "python, golang, typescript"

    assert_equal 3, @article.tags.count
    assert @article.tags.pluck(:name).include?("python")
    assert @article.tags.pluck(:name).include?("golang")
    assert @article.tags.pluck(:name).include?("typescript")
  end

  test "tag_list= should reuse existing tags" do
    existing_tag = tags(:ruby)
    @article.save!
    @article.tag_list = "ruby, new-tag"

    assert_equal 2, @article.tags.count
    assert_includes @article.tags, existing_tag
  end

  test "search_content should find articles by title" do
    article = articles(:published_article)
    results = Article.search_content("Published")
    assert_includes results, article
  end

  test "search_content should find articles by slug" do
    article = articles(:published_article)
    results = Article.search_content("published-article")
    assert_includes results, article
  end

  test "search_content should return all when query is blank" do
    results = Article.search_content("")
    assert_equal Article.count, results.count
  end

  test "should have many comments" do
    article = articles(:published_article)
    assert_respond_to article, :comments
  end

  test "should have many tags through article_tags" do
    article = articles(:published_article)
    assert_respond_to article, :tags
  end

  test "should have many social_media_posts" do
    article = articles(:published_article)
    assert_respond_to article, :social_media_posts
  end

  test "should destroy associated comments when destroyed" do
    article = Article.create!(
      title: "Article to destroy",
      slug: "article-to-destroy-comments",
      status: :draft,
      content_type: :html,
      html_content: "<p>Content</p>"
    )
    article.comments.create!(
      author_name: "Test",
      content: "Test comment",
      status: :approved
    )

    assert_difference "Comment.count", -1 do
      article.destroy
    end
  end

  test "should destroy associated article_tags when destroyed" do
    article = Article.create!(
      title: "Article to destroy",
      slug: "article-to-destroy-tags",
      status: :draft,
      content_type: :html,
      html_content: "<p>Content</p>"
    )
    tag = tags(:ruby)
    article.tags << tag

    assert_difference "ArticleTag.count", -1 do
      article.destroy
    end
  end

  test "renders content and derives SEO helpers" do
    html_article = Article.create!(
      title: "HTML Article",
      slug: "html-article",
      status: :publish,
      content_type: :html,
      html_content: "<p>Hello <strong>World</strong></p>",
      description: "html desc"
    )

    assert_equal html_article.html_content, html_article.rendered_content
    assert_equal "Hello World", html_article.plain_text_content

    html_article.meta_title = "Meta Title"
    html_article.meta_description = "Meta Desc"
    html_article.meta_image = "https://example.com/meta.png"
    assert_equal "Meta Title", html_article.seo_meta_title
    assert_equal "Meta Desc", html_article.seo_meta_description
    assert_equal "https://example.com/meta.png", html_article.seo_meta_image
    assert articles(:source_article).has_source?

    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("fake-image"),
      filename: "image.png",
      content_type: "image/png"
    )
    attachment = ActionText::Attachment.from_attachable(blob)
    rich_article = Article.create!(
      title: "Rich Article",
      slug: "rich-article",
      status: :publish,
      content: "<p>Hi</p>#{attachment.to_html}",
      description: "rich desc"
    )

    assert_equal blob, rich_article.first_image_attachment
    assert_includes rich_article.rendered_content.to_s, "Hi"
    assert_includes rich_article.plain_text_content, "Hi"
    assert_equal Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true), rich_article.seo_meta_image

    long_text = "a" * 200
    long_article = Article.create!(
      title: "Long Article",
      slug: "long-article",
      status: :publish,
      content_type: :html,
      html_content: "<p>#{long_text}</p>",
      description: "long desc"
    )
    assert_equal "Long Article", long_article.seo_meta_title
    assert long_article.seo_meta_description.end_with?("...")
    assert_equal 160, long_article.seo_meta_description.length

    fallback_article = Article.new(description: "fallback desc")
    assert_equal "fallback desc", fallback_article.seo_meta_description
  end

  test "extracts TinyMCE images from HTML content for crosspost" do
    # Create blob for TinyMCE uploaded image
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("fake-tinymce-image"),
      filename: "tinymce-image.png",
      content_type: "image/png"
    )

    # Simulate TinyMCE content with image URL inserted
    image_url = Rails.application.routes.url_helpers.rails_blob_url(blob, host: "example.com")
    tinymce_article = Article.create!(
      title: "TinyMCE Article",
      slug: "tinymce-article",
      status: :publish,
      content: "<p>Hello World</p><img src='#{image_url}' alt='test image'><p>More content</p>",
      description: "tinymce desc"
    )

    # first_image_attachment should find the blob from TinyMCE image URL
    result = tinymce_article.first_image_attachment
    assert result.is_a?(ActiveStorage::Blob), "Expected ActiveStorage::Blob but got #{result.class}"
    assert_equal blob.id, result.id

    # SEO meta image should also work
    assert_equal Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true), tinymce_article.seo_meta_image
  end

  test "extracts remote images from HTML when blob not found" do
    remote_article = Article.create!(
      title: "Remote Image Article",
      slug: "remote-image-article",
      status: :publish,
      content: '<p>Check this out</p><img src="https://example.com/external-image.jpg" alt="external"><p>End</p>',
      description: "remote desc"
    )

    # first_image_attachment should return RemoteImageWrapper for external URL
    result = remote_article.first_image_attachment
    assert result.is_a?(RemoteImageWrapper), "Expected RemoteImageWrapper but got #{result.class}"
    assert_equal "https://example.com/external-image.jpg", result.url
    assert_equal "ActionText::Attachables::RemoteImage", result.class.name

    # SEO meta image should return the remote URL
    assert_equal "https://example.com/external-image.jpg", remote_article.seo_meta_image
  end

  test "handles articles without images for crosspost" do
    no_image_article = Article.create!(
      title: "No Image Article",
      slug: "no-image-article",
      status: :publish,
      content: "<p>Just plain text content</p>",
      description: "no image desc"
    )

    assert_nil no_image_article.first_image_attachment
    assert_nil no_image_article.seo_meta_image
  end

  test "handles scheduling helpers, crosspost checks, and post cleanup" do
    setting = settings(:default)
    original_zone = setting.time_zone
    setting.update!(time_zone: "Pacific Time (US & Canada)")
    CacheableSettings.refresh_site_info

    article = Article.new(
      title: "Scheduled",
      slug: "scheduled-helper",
      status: :schedule,
      scheduled_at: Time.utc(2025, 1, 1, 12, 0, 0),
      content_type: :html,
      html_content: "<p>Content</p>"
    )
    article.send(:handle_time_zone)
    assert article.scheduled_at.utc?

    crosspost = Crosspost.mastodon
    crosspost.update!(
      enabled: true,
      server_url: "https://mastodon.social",
      client_key: "client",
      client_secret: "secret",
      access_token: "token"
    )
    article.crosspost_mastodon = "1"
    assert article.send(:should_crosspost_to?, "mastodon")

    article.social_media_posts.build(url: "")
    article.social_media_posts.build(url: "https://example.com/post")
    article.send(:cleanup_empty_social_media_posts)
    assert article.social_media_posts.first.marked_for_destruction?
  ensure
    setting.update!(time_zone: original_zone) if setting&.persisted?
    CacheableSettings.refresh_site_info
  end

  test "newsletter helpers respect publish status and enqueue jobs" do
    draft = Article.new(status: :draft)
    assert_not draft.send(:should_send_newsletter?)

    newsletter_setting = NewsletterSetting.instance
    newsletter_setting.update!(
      provider: "native",
      enabled: true,
      smtp_address: "smtp.example.com",
      smtp_port: 587,
      smtp_user_name: "user",
      smtp_password: "pass",
      from_email: "from@example.com"
    )

    article = Article.create!(
      title: "Newsletter Article",
      slug: "newsletter-article",
      status: :publish,
      content_type: :html,
      html_content: "<p>Body</p>"
    )
    article.send_newsletter = "1"
    assert article.send(:should_send_newsletter?)

    assert_enqueued_with(job: NativeNewsletterSenderJob, args: [ article.id ]) do
      article.send(:handle_newsletter)
    end
  end

  # Crosspost job tests
  test "should enqueue crosspost job when publishing with crosspost enabled" do
    # Setup: enable mastodon crosspost platform
    Crosspost.find_or_create_by(platform: "mastodon").update!(
      enabled: true,
      client_key: "test_key",
      client_secret: "test_secret",
      access_token: "test_token"
    )

    article = Article.new(
      title: "Crosspost Test Article",
      slug: "crosspost-test-publish",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>",
      crosspost_mastodon: "1"
    )

    assert_enqueued_with(job: CrosspostArticleJob) do
      article.save!
    end
  end

  test "should enqueue crosspost job when saving as shared with crosspost enabled" do
    # Setup: enable mastodon crosspost platform
    Crosspost.find_or_create_by(platform: "mastodon").update!(
      enabled: true,
      client_key: "test_key",
      client_secret: "test_secret",
      access_token: "test_token"
    )

    article = Article.new(
      title: "Shared Crosspost Test",
      slug: "crosspost-test-shared",
      status: :shared,
      content_type: :html,
      html_content: "<p>Shared content</p>",
      crosspost_mastodon: "1"
    )

    # shared status should NOT trigger crosspost (only publish does)
    assert_no_enqueued_jobs(only: CrosspostArticleJob) do
      article.save!
    end
  end

  test "should not enqueue crosspost job when crosspost not checked" do
    # Setup: enable mastodon crosspost platform
    Crosspost.find_or_create_by(platform: "mastodon").update!(
      enabled: true,
      client_key: "test_key",
      client_secret: "test_secret",
      access_token: "test_token"
    )

    article = Article.new(
      title: "No Crosspost Article",
      slug: "no-crosspost-test",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>",
      crosspost_mastodon: "0"  # Not checked
    )

    assert_no_enqueued_jobs(only: CrosspostArticleJob) do
      article.save!
    end
  end

  test "should not enqueue crosspost job when platform not enabled" do
    # Setup: disable mastodon crosspost platform
    Crosspost.find_or_create_by(platform: "mastodon").update!(enabled: false)

    article = Article.new(
      title: "Platform Disabled Article",
      slug: "platform-disabled-test",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>",
      crosspost_mastodon: "1"  # Checked but platform disabled
    )

    assert_no_enqueued_jobs(only: CrosspostArticleJob) do
      article.save!
    end
  end

  test "should not enqueue crosspost job for xiaohongshu" do
    Crosspost.find_or_create_by(platform: "xiaohongshu").update!(enabled: true)

    article = Article.new(
      title: "Xiaohongshu Crosspost Test",
      slug: "xiaohongshu-crosspost-test",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>",
      crosspost_xiaohongshu: "1"
    )

    assert_no_enqueued_jobs(only: CrosspostArticleJob) do
      article.save!
    end
  end

  test "should enqueue multiple crosspost jobs for multiple platforms" do
    # Setup: enable multiple crosspost platforms
    Crosspost.find_or_create_by(platform: "mastodon").update!(
      enabled: true,
      client_key: "test_key",
      client_secret: "test_secret",
      access_token: "test_token"
    )
    Crosspost.find_or_create_by(platform: "bluesky").update!(
      enabled: true,
      username: "test@bsky.social",
      app_password: "test_password"
    )

    article = Article.new(
      title: "Multi Crosspost Article",
      slug: "multi-crosspost-test",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>",
      crosspost_mastodon: "1",
      crosspost_bluesky: "1"
    )

    assert_difference -> { enqueued_jobs.count { |j| j["job_class"] == "CrosspostArticleJob" } }, 2 do
      article.save!
    end
  end

  # Scheduled article crosspost tests
  test "should enqueue crosspost job when scheduled article is published" do
    # Setup: enable mastodon crosspost platform
    Crosspost.find_or_create_by(platform: "mastodon").update!(
      enabled: true,
      client_key: "test_key",
      client_secret: "test_secret",
      access_token: "test_token"
    )

    # Create scheduled article with crosspost enabled
    article = Article.create!(
      title: "Scheduled Crosspost Article",
      slug: "scheduled-crosspost-test",
      status: :schedule,
      scheduled_at: 1.hour.ago,  # Already past scheduled time
      content_type: :html,
      html_content: "<p>Scheduled content</p>",
      crosspost_mastodon: "1"
    )

    # Verify no crosspost job is enqueued during scheduling
    assert_no_enqueued_jobs(only: CrosspostArticleJob)

    # Now set the crosspost flag again and call publish_scheduled
    article.crosspost_mastodon = "1"

    # When publish_scheduled is called, it should trigger crosspost
    assert_enqueued_with(job: CrosspostArticleJob) do
      article.publish_scheduled
    end
  end

  test "should schedule publication job for scheduled article" do
    article = Article.new(
      title: "Schedule Job Test",
      slug: "schedule-job-test",
      status: :schedule,
      scheduled_at: 1.hour.from_now,
      content_type: :html,
      html_content: "<p>Scheduled content</p>"
    )

    assert_enqueued_with(job: PublishScheduledArticlesJob) do
      article.save!
    end
  end

  test "scheduled article should transition to publish status when publish_scheduled is called" do
    # Setup: enable mastodon crosspost platform
    Crosspost.find_or_create_by(platform: "mastodon").update!(
      enabled: true,
      client_key: "test_key",
      client_secret: "test_secret",
      access_token: "test_token"
    )

    # Create article with schedule status and past scheduled_at
    article = Article.create!(
      title: "Transition Test Article",
      slug: "transition-test",
      status: :schedule,
      scheduled_at: 1.hour.ago,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )

    # Set crosspost flag before publishing
    article.crosspost_mastodon = "1"

    # Call publish_scheduled (this happens when PublishScheduledArticlesJob runs)
    article.publish_scheduled

    # Verify article is now published
    assert article.publish?, "Article should be in publish status"
    assert_nil article.scheduled_at, "scheduled_at should be nil after publishing"
  end
end
