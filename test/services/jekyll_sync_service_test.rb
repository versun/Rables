# frozen_string_literal: true

require "test_helper"

class JekyllSyncServiceTest < ActiveSupport::TestCase
  test "sync_all exports articles and pages" do
    dir = Dir.mktmpdir
    setting = JekyllSetting.create!(jekyll_path: dir)
    create_published_article(title: "Sync Article", slug: "sync-article")
    Page.create!(
      title: "Sync Page",
      slug: "sync-page",
      status: :publish,
      content_type: :html,
      html_content: "<p>Sync page</p>"
    )

    service = JekyllSyncService.new(setting: setting)
    result = service.sync_all

    assert_equal Article.count, result[:articles_count]
    assert_equal Page.count, result[:pages_count]

    posts = Dir.glob(File.join(dir, setting.posts_directory, "*.md"))
    pages = Dir.glob(File.join(dir, setting.pages_directory, "*.md"))

    assert_equal Article.count, posts.size
    assert_equal Page.count, pages.size
  ensure
    FileUtils.remove_entry(dir) if dir
  end
end
