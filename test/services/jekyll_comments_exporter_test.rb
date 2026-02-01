# frozen_string_literal: true

require "test_helper"

class JekyllCommentsExporterTest < ActiveSupport::TestCase
  test "exports approved comments to data file" do
    dir = Dir.mktmpdir
    JekyllSetting.create!(jekyll_path: dir)

    article = create_published_article(title: "Comment Export", slug: "comment-export")
    Comment.create!(
      commentable: article,
      author_name: "Tester",
      content: "Great post!",
      status: :approved
    )

    exporter = JekyllCommentsExporter.new
    exporter.export_for_article(article)

    data_path = File.join(dir, "_data", "comments", "comment-export.yml")
    assert File.exist?(data_path)
    assert_includes File.read(data_path), "Great post!"
  ensure
    FileUtils.remove_entry(dir) if dir
  end
end
