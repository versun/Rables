# frozen_string_literal: true

require "test_helper"

class JekyllAttachmentProcessorTest < ActiveSupport::TestCase
  test "copies active storage and remote images into jekyll assets" do
    dir = Dir.mktmpdir
    setting = JekyllSetting.create!(jekyll_path: dir)
    processor = JekyllAttachmentProcessor.new(setting: setting)

    article = create_published_article(slug: "asset-article")
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("blob-data"),
      filename: "blob.png",
      content_type: "image/png"
    )
    blob_url = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)

    html = <<~HTML
      <p>Intro</p>
      <img src="#{blob_url}" alt="Blob">
      <img src="http://example.com/remote.jpg" alt="Remote">
    HTML

    uri_stub = lambda do |_url, **_kwargs, &block|
      io = StringIO.new("remote-image")
      block ? block.call(io) : io
    end

    original_open = URI.method(:open)
    URI.define_singleton_method(:open, uri_stub)

    begin
      processed = processor.process_article_attachments(article, html)
      assert_includes processed, "/assets/images/posts/asset-article/"

      exported_dir = File.join(dir, "assets", "images", "posts", "asset-article")
      assert Dir.exist?(exported_dir)
      assert Dir.glob(File.join(exported_dir, "*")).any?
    ensure
      URI.define_singleton_method(:open, original_open)
      FileUtils.remove_entry(dir) if dir
    end
  end
end
