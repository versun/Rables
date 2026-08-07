# frozen_string_literal: true

require "test_helper"
require "minitest/mock"
require "digest"
require "json"

class SiteExportTest < ActiveSupport::TestCase
  SILENT_LOGGER = ->(_message) { }

  setup do
    @output_dir = Dir.mktmpdir("site_export_test")

    @blob = ActiveStorage::Blob.create_and_upload!(
      io: file_fixture("test_image.png").open,
      filename: "test_image.png",
      content_type: "image/png"
    )

    @tag = create_tag(name: "migration-tag")
    @article = Article.new(title: "Export Me", slug: "export-me", status: :publish, description: "desc")
    @article.content = %(<p>Hello</p><action-text-attachment sgid="#{@blob.attachable_sgid}"></action-text-attachment>)
    @article.save!
    @article.tags << @tag

    blob_path = Rails.application.routes.url_helpers.rails_blob_path(@blob, only_path: true)
    @html_article = create_published_article(
      slug: "export-html",
      html_content: %(<p>HTML <img src="#{blob_path}"></p>)
    )

    @page = Page.create!(title: "About", slug: "export-about", status: :publish, content_type: :html, html_content: "<p>about</p>")
    @comment = Comment.create!(commentable: @article, author_name: "Jane", content: "Nice", status: :approved)
    @subscriber = create_subscriber(email: "migration@example.com")
    @subscriber.tags << @tag
    @redirect = Redirect.create!(regex: "^/old$", replacement: "/new", enabled: true, permanent: true)
  end

  teardown do
    FileUtils.rm_rf(@output_dir)
  end

  test "export creates the full package structure" do
    run_export

    %w[articles.jsonl pages.jsonl tags.jsonl article_tags.jsonl comments.jsonl redirects.jsonl subscribers.jsonl blobs.jsonl url_map.jsonl].each do |name|
      assert File.exist?(File.join(@output_dir, "data", name)), "missing data/#{name}"
    end
    assert File.exist?(File.join(@output_dir, "articles", "export-me.html"))
    assert File.exist?(File.join(@output_dir, "articles", "export-me.source.html"))
    assert File.exist?(File.join(@output_dir, "pages", "export-about.html"))
    assert File.exist?(File.join(@output_dir, "images", "#{@blob.key}.png"))
  end

  test "rendered content rewrites media URLs to local image paths" do
    run_export

    html = File.read(File.join(@output_dir, "articles", "export-me.html"))
    assert_includes html, "/images/#{@blob.key}.png"
    assert_not_includes html, "/rails/active_storage"
    assert_not_includes html, "action-text-attachment"

    html_article_html = File.read(File.join(@output_dir, "articles", "export-html.html"))
    assert_includes html_article_html, %(<img src="/images/#{@blob.key}.png")
  end

  test "source file preserves the untouched original markup" do
    run_export

    source = File.read(File.join(@output_dir, "articles", "export-me.source.html"))
    assert_includes source, "action-text-attachment"
  end

  test "exported image file matches the blob checksum and content" do
    run_export

    file = File.join(@output_dir, "images", "#{@blob.key}.png")
    assert_equal @blob.checksum, Digest::MD5.file(file).base64digest
    assert_equal file_fixture("test_image.png").binread, File.binread(file)
  end

  test "articles.jsonl carries the migration metadata" do
    run_export

    row = read_jsonl("articles.jsonl").find { |r| r["slug"] == "export-me" }
    assert_equal "publish", row["status"]
    assert_equal "rich_text", row["source_content_type"]
    assert_equal "articles/export-me.html", row["content_file"]
    assert_equal "articles/export-me.source.html", row["source_file"]
    assert_equal @article.created_at.utc.iso8601, row["created_at"]

    html_row = read_jsonl("articles.jsonl").find { |r| r["slug"] == "export-html" }
    assert_equal "html", html_row["source_content_type"]
  end

  test "related data is exported with slug references" do
    run_export

    article_tags = read_jsonl("article_tags.jsonl").map { |r| [ r["article_slug"], r["tag_slug"] ] }
    assert_includes article_tags, [ "export-me", "migration-tag" ]

    comment_row = read_jsonl("comments.jsonl").find { |r| r["id"] == @comment.id }
    assert_equal "Article", comment_row["commentable_type"]
    assert_equal "export-me", comment_row["commentable_slug"]
    assert_equal "approved", comment_row["status"]

    subscriber_row = read_jsonl("subscribers.jsonl").find { |r| r["email"] == "migration@example.com" }
    assert_equal [ "migration-tag" ], subscriber_row["tag_slugs"]
    assert_equal @subscriber.confirmation_token, subscriber_row["confirmation_token"]

    tag_row = read_jsonl("tags.jsonl").find { |r| r["slug"] == "migration-tag" }
    assert_equal "migration-tag", tag_row["name"]

    redirect_row = read_jsonl("redirects.jsonl").find { |r| r["regex"] == "^/old$" }
    assert_equal "/new", redirect_row["replacement"]
    assert_equal true, redirect_row["permanent"]
  end

  test "url_map maps legacy active storage URLs to exported files" do
    run_export

    rows = read_jsonl("url_map.jsonl")
    assert rows.any?, "expected at least one legacy URL mapping"
    rows.each do |row|
      assert_includes row["old_path"], "/rails/active_storage/"
      assert row["new_path"].start_with?("/images/")
    end
    assert(rows.any? { |r| r["new_path"] == "/images/#{@blob.key}.png" })
  end

  test "rerunning the export into the same directory is idempotent" do
    run_export
    first = File.read(File.join(@output_dir, "data", "blobs.jsonl"))

    run_export
    assert_equal first, File.read(File.join(@output_dir, "data", "blobs.jsonl"))
  end

  test "rerun re-downloads files that fail checksum verification" do
    run_export
    file = File.join(@output_dir, "images", "#{@blob.key}.png")
    File.binwrite(file, "corrupted")

    run_export

    assert_equal file_fixture("test_image.png").binread, File.binread(file)
  end

  test "downloads remote images referenced by content" do
    url = "https://cdn.example.com/images/pic.png"
    create_published_article(slug: "remote-image", html_content: %(<p><img src="#{url}"></p>))
    fake = FakeRemoteResponse.new(file_fixture("test_image.png").binread, "image/png")

    URI.stub(:open, fake) { run_export }

    filename = "remote-#{Digest::MD5.hexdigest(url)}.png"
    html = File.read(File.join(@output_dir, "articles", "remote-image.html"))
    assert_includes html, %(<img src="/images/#{filename}")
    assert_equal file_fixture("test_image.png").binread, File.binread(File.join(@output_dir, "images", filename))

    row = read_jsonl("blobs.jsonl").find { |r| r["remote_url"] == url }
    assert_equal filename, File.basename(row["file"])
    assert_equal "image/png", row["content_type"]
    assert_equal Digest::MD5.base64digest(fake.data), row["checksum_base64_md5"]
  end

  test "sniffs the media type when the remote server returns octet-stream" do
    url = "https://cdn.example.com/download.php?id=1"
    create_published_article(slug: "octet-image", html_content: %(<p><img src="#{url}"></p>))
    fake = FakeRemoteResponse.new(file_fixture("test_image.png").binread, "application/octet-stream")

    URI.stub(:open, fake) { run_export }

    filename = "remote-#{Digest::MD5.hexdigest(url)}.png"
    html = File.read(File.join(@output_dir, "articles", "octet-image.html"))
    assert_includes html, "/images/#{filename}"
  end

  test "leaves non-media remote URLs untouched" do
    url = "https://example.com/page.html"
    create_published_article(slug: "non-media", html_content: %(<p><img src="#{url}"></p>))
    fake = FakeRemoteResponse.new("<html><body>nope</body></html>", "text/html")

    URI.stub(:open, fake) { run_export }

    html = File.read(File.join(@output_dir, "articles", "non-media.html"))
    assert_includes html, url
  end

  test "leaves unresolvable active storage URLs untouched" do
    broken = "/rails/active_storage/blobs/redirect/invalid--signature/x.png"
    create_published_article(slug: "broken-blob", html_content: %(<p><img src="#{broken}"></p>))

    run_export

    html = File.read(File.join(@output_dir, "articles", "broken-blob.html"))
    assert_includes html, broken
  end

  test "maps variant blob URLs to the original image" do
    original = ActiveStorage::Blob.create_and_upload!(
      io: file_fixture("test_image.jpg").open,
      filename: "photo.jpg",
      content_type: "image/jpeg"
    )
    variant_blob = ActiveStorage::Blob.create_and_upload!(
      io: file_fixture("test_image.jpg").open,
      filename: "photo_variant.jpg",
      content_type: "image/jpeg"
    )
    variant_record = ActiveStorage::VariantRecord.create!(blob: original, variation_digest: "test-digest")
    variant_record.image.attach(variant_blob)

    url = "https://s3.example.com/bucket/#{variant_blob.key}"
    create_published_article(slug: "variant-image", html_content: %(<p><img src="#{url}"></p>))

    run_export

    html = File.read(File.join(@output_dir, "articles", "variant-image.html"))
    assert_includes html, "/images/#{original.key}.jpg"
    assert File.exist?(File.join(@output_dir, "images", "#{original.key}.jpg"))
    assert_nil(read_jsonl("blobs.jsonl").find { |r| r["key"] == variant_blob.key })
  end

  # The site sanitizer (Sanitization::ALLOWED_HTML_ATTRIBUTES) strips srcset
  # and poster before rendering, so full-pipeline exports never contain them;
  # exercise the rewriting at the rewrite_media level instead.
  test "rewrite_media rewrites srcset candidates and video poster" do
    second_blob = ActiveStorage::Blob.create_and_upload!(
      io: file_fixture("test_image.png").open,
      filename: "second.png",
      content_type: "image/png"
    )
    blob_path = Rails.application.routes.url_helpers.rails_blob_path(@blob, only_path: true)
    second_path = Rails.application.routes.url_helpers.rails_blob_path(second_blob, only_path: true)
    html = %(<img src="#{blob_path}" srcset="#{blob_path} 1x, #{second_path} 2x">) +
           %(<video poster="#{second_path}"></video>)

    rewritten = rewrite_media(html)

    assert_includes rewritten, %(srcset="/images/#{@blob.key}.png 1x, /images/#{second_blob.key}.png 2x")
    assert_includes rewritten, %(poster="/images/#{second_blob.key}.png")
    assert_not_includes rewritten, "/rails/active_storage"
  end

  test "rewrite_media leaves data URIs and unresolvable srcset URLs untouched" do
    data_uri = "data:image/svg+xml;base64,PHN2Zy8+"
    html = %(<img srcset="#{data_uri} 1x, https://example.com/missing.png 2x">)
    fake = FakeRemoteResponse.new("<html>nope</html>", "text/html")

    rewritten = URI.stub(:open, fake) { rewrite_media(html) }

    assert_includes rewritten, data_uri
    assert_includes rewritten, "https://example.com/missing.png"
  end

  test "skips remote images that exceed the download size limit" do
    url = "https://cdn.example.com/images/huge.png"
    create_published_article(slug: "huge-image", html_content: %(<p><img src="#{url}"></p>))
    fake = FakeRemoteResponse.new(file_fixture("test_image.png").binread, "image/png")

    URI.stub(:open, fake) { run_export(max_remote_download_bytes: 10) }

    html = File.read(File.join(@output_dir, "articles", "huge-image.html"))
    assert_includes html, url
    assert_empty Dir.children(File.join(@output_dir, "images")).grep(/^remote-/)
  end

  private

  # Minimal stand-in for the object open-uri returns (OpenURI::Meta).
  class FakeRemoteResponse
    attr_reader :data, :content_type

    def initialize(data, content_type)
      @data = data
      @content_type = content_type
    end

    def read(length = nil) = length ? @data.byteslice(0, length) : @data
  end

  def run_export(**options)
    SiteExport.export(output_dir: @output_dir, logger: SILENT_LOGGER, **options)
  end

  # Directly exercise the media-rewriting step on a prepared exporter.
  def rewrite_media(html)
    exporter = SiteExport.new(output_dir: @output_dir, logger: SILENT_LOGGER)
    exporter.send(:prepare_directories)
    exporter.send(:rewrite_media, html, context: "test")
  end

  def read_jsonl(name)
    File.readlines(File.join(@output_dir, "data", name), chomp: true).reject(&:empty?).map { |line| JSON.parse(line) }
  end
end
