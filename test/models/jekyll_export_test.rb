# frozen_string_literal: true

require "test_helper"

class JekyllExportTest < ActiveSupport::TestCase
  test "exports article and page markdown into jekyll directories" do
    dir = Dir.mktmpdir
    setting = JekyllSetting.create!(jekyll_path: dir)
    exporter = JekyllExport.new(setting: setting)

    article = create_published_article(title: "Hello Jekyll", slug: "hello-jekyll")
    page = Page.create!(
      title: "About",
      slug: "about",
      status: :publish,
      content_type: :html,
      html_content: "<p>About page</p>"
    )

    exporter.export_article(article)
    exporter.export_page(page)

    posts = Dir.glob(File.join(dir, setting.posts_directory, "*.md"))
    pages = Dir.glob(File.join(dir, setting.pages_directory, "*.md"))

    assert_equal 1, posts.size
    assert_equal 1, pages.size

    post_content = File.read(posts.first)
    assert_includes post_content, "title: Hello Jekyll"
    assert_includes post_content, "rables_id: #{article.id}"
    assert_includes post_content, "layout: post"

    page_content = File.read(pages.first)
    assert_includes page_content, "title: About"
    assert_includes page_content, "rables_id: #{page.id}"
    assert_includes page_content, "layout: page"
  ensure
    FileUtils.remove_entry(dir) if dir
  end

  test "adds permalink when article_route_prefix is set" do
    dir = Dir.mktmpdir
    setting = JekyllSetting.create!(jekyll_path: dir)
    exporter = JekyllExport.new(setting: setting)
    article = create_published_article(title: "Permalink Article", slug: "permalink-article")

    original_prefix = Rails.application.config.x.article_route_prefix
    Rails.application.config.x.article_route_prefix = "blog"

    exporter.export_article(article)
    posts = Dir.glob(File.join(dir, setting.posts_directory, "*.md"))
    content = File.read(posts.first)

    assert_includes content, "permalink: \"/blog/permalink-article\""
  ensure
    Rails.application.config.x.article_route_prefix = original_prefix
    FileUtils.remove_entry(dir) if dir
  end
end
