# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class ExportTest < ActiveSupport::TestCase
  test "exports article source reference and seo fields" do
    slug = "export-seo-source-#{SecureRandom.hex(6)}"
    article = create_published_article(
      slug: slug,
      source_url: "https://example.com/source",
      source_author: "Example Author",
      source_content: "Example quote",
      meta_title: "Example Meta Title",
      meta_description: "Example Meta Description",
      meta_image: "https://example.com/meta.jpg"
    )

    exporter = Export.new

    assert exporter.generate, exporter.error_message
    assert exporter.zip_path.present?
    assert File.exist?(exporter.zip_path), "expected zip to exist at #{exporter.zip_path}"

    Zip::File.open(exporter.zip_path) do |zip|
      entry = zip.find_entry("articles.csv")
      assert entry, "expected articles.csv to exist in zip"

      content = entry.get_input_stream.read
      rows = CSV.parse(content, headers: true)
      exported = rows.find { |row| row["slug"] == slug }
      assert exported, "expected article row to be exported for slug #{slug}"

      assert_equal article.source_url, exported["source_url"]
      assert_equal article.source_author, exported["source_author"]
      assert_equal article.source_content, exported["source_content"]
      assert_equal article.meta_title, exported["meta_title"]
      assert_equal article.meta_description, exported["meta_description"]
      assert_equal article.meta_image, exported["meta_image"]
    end
  ensure
    File.delete(exporter.zip_path) if exporter&.zip_path.present? && File.exist?(exporter.zip_path)
    article&.destroy
  end

  test "includes git_integrations.csv in export zip" do
    exporter = Export.new

    assert exporter.generate, exporter.error_message
    assert exporter.zip_path.present?
    assert File.exist?(exporter.zip_path), "expected zip to exist at #{exporter.zip_path}"

    Zip::File.open(exporter.zip_path) do |zip|
      entry = zip.find_entry("git_integrations.csv")
      assert entry, "expected git_integrations.csv to exist in zip"
      content = entry.get_input_stream.read
      assert_includes content, "provider"
      assert_includes content, "github"
    end
  ensure
    File.delete(exporter.zip_path) if exporter&.zip_path.present? && File.exist?(exporter.zip_path)
  end

  test "processes html attachments and remote images" do
    exporter = Export.new
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("blob-data"),
      filename: "blob.png",
      content_type: "image/png"
    )
    blob_url = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)

    html = <<~HTML
      <p>Intro</p>
      <action-text-attachment content-type="image/png" url="#{blob_url}" filename="blob.png"></action-text-attachment>
      <figure data-trix-attachment='{"contentType":"image/png","url":"http://example.com/figure.jpg","filename":"figure.jpg"}' data-trix-attributes='{"caption":"Figure"}'></figure>
      <img src="http://example.com/remote.jpg">
      <img src="http://example.com/remote">
      <img src="/uploads/relative.jpg">
    HTML

    head_response = Net::HTTPOK.new("1.1", "200", "OK")
    head_response["Content-Type"] = "image/png"
    head_response.instance_variable_set(:@read, true)
    http = Struct.new(:response) { def head(_url) response end }.new(head_response)

    uri_stub = lambda do |_url, **_kwargs, &block|
      io = StringIO.new("remote-image")
      block ? block.call(io) : io
    end

    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &block) { block.call(http) }) do
      URI.stub(:open, uri_stub) do
        processed = exporter.process_html_content(html, record_id: 1, record_type: "article")
        assert_includes processed, "attachments/article_1"
        assert Dir.exist?(exporter.attachments_dir)
      end
    end
  ensure
    FileUtils.rm_rf(exporter.export_dir) if exporter&.export_dir
  end

  test "exports rich text content and related records" do
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("blob-data"),
      filename: "blob.png",
      content_type: "image/png"
    )
    attachment = ActionText::Attachment.from_attachable(blob)
    rich_article = Article.create!(
      title: "Rich Export",
      slug: "rich-export-#{SecureRandom.hex(4)}",
      status: :publish,
      content: "<p>Body</p>#{attachment.to_html}"
    )

    tag = Tag.create!(name: "Export Tag #{SecureRandom.hex(3)}", slug: "export-tag-#{SecureRandom.hex(3)}")
    ArticleTag.create!(article: rich_article, tag: tag)

    subscriber = Subscriber.create!(
      email: "export-#{SecureRandom.hex(4)}@example.com",
      confirmation_token: SecureRandom.urlsafe_base64(32),
      unsubscribe_token: SecureRandom.urlsafe_base64(32)
    )
    SubscriberTag.create!(subscriber: subscriber, tag: tag)

    SocialMediaPost.create!(article: rich_article, platform: "twitter", url: "https://example.com/post")
    Redirect.create!(regex: "^/export$", replacement: "/exported", enabled: true, permanent: false)

    static_file = StaticFile.new(filename: "export.txt", description: "export")
    file = File.open(Rails.root.join("test/fixtures/files/sample.txt"))
    static_file.file.attach(io: file, filename: "export.txt", content_type: "text/plain")
    static_file.save!
    file.close

    newsletter_setting = NewsletterSetting.create!(provider: "native", enabled: false)
    newsletter_setting.update!(footer: "<p>Footer</p>")

    exporter = Export.new
    assert exporter.generate, exporter.error_message

    Zip::File.open(exporter.zip_path) do |zip|
      assert zip.find_entry("static_files.csv")
      assert zip.find_entry("newsletter_settings.csv")
      assert zip.find_entry("social_media_posts.csv")
      assert zip.find_entry("subscriber_tags.csv")
    end
  ensure
    File.delete(exporter.zip_path) if exporter&.zip_path.present? && File.exist?(exporter.zip_path)
  end

  test "handles export failures and activity log export" do
    ActiveRecord::Base.connection.stub(:execute, ->(_sql) { raise "db down" }) do
      failing_exporter = Export.new
      assert_not failing_exporter.check_database_connection
      assert_match "Database connection failed", failing_exporter.error_message
      FileUtils.rm_rf(failing_exporter.export_dir) if failing_exporter.export_dir
    end

    ActivityLog.create!(action: "test", target: "export", level: :info, description: "log")
    exporter = Export.new
    exporter.send(:export_activity_logs)
    assert File.exist?(File.join(exporter.export_dir, "activity_logs.csv"))

    exporter.stub(:export_articles, -> { raise "boom" }) do
      assert_not exporter.generate
      assert_match "boom", exporter.error_message
    end
  ensure
    FileUtils.rm_rf(exporter.export_dir) if exporter&.export_dir
  end

  test "processes attachment nodes and fallback article content" do
    exporter = Export.new
    exporter.stub(:download_and_save_attachment, "attachments/test.png") do
      attachment = Nokogiri::HTML.fragment(
        "<action-text-attachment content-type=\"image/png\" url=\"http://example.com/a.png\" filename=\"a.png\" caption=\"Cap\"><img></action-text-attachment>"
      ).at_css("action-text-attachment")
      exporter.send(:process_attachment_element, attachment, 1, "article")
      assert_equal "attachments/test.png", attachment["url"]
      assert_equal "Cap", attachment.at_css("img")["alt"]

      fragment = Nokogiri::HTML.fragment(
        "<action-text-attachment content-type=\"image/png\" url=\"http://example.com/a.png\" filename=\"a.png\"></action-text-attachment>"
      )
      attachment_no_img = fragment.at_css("action-text-attachment")
      exporter.send(:process_attachment_element, attachment_no_img, 1, "article")
      assert_includes fragment.to_html, "<img"

      figure_json = { contentType: "image/png", url: "http://example.com/f.png", filename: "f.png" }.to_json
      figure = Nokogiri::HTML.fragment("<figure data-trix-attachment='#{figure_json}' data-trix-attributes='{\"caption\":\"Fig\"}'></figure>").at_css("figure")
      exporter.send(:process_figure_element, figure, 1, "article")
      assert_includes figure.to_html, "<img"
    end

    empty_rich_article = Article.new(content_type: :rich_text)
    assert_equal "", exporter.send(:process_article_content, empty_rich_article)
  ensure
    FileUtils.rm_rf(exporter.export_dir) if exporter&.export_dir
  end

  test "skips static files without attachments" do
    static_file = StaticFile.new(filename: "missing.txt")
    static_file.save!(validate: false)

    exporter = Export.new
    exporter.send(:export_static_files)

    csv_path = File.join(exporter.export_dir, "static_files.csv")
    assert File.exist?(csv_path)
    assert_equal 1, File.readlines(csv_path).size

    attachments_path = File.join(exporter.attachments_dir, "static_files")
    refute Dir.exist?(attachments_path)
  ensure
    FileUtils.rm_rf(exporter.export_dir) if exporter&.export_dir
    static_file&.delete
  end

  test "cleans up old export and import files" do
    Dir.mktmpdir do |tmp_root|
      Rails.stub(:root, Pathname.new(tmp_root)) do
        exports_dir = Rails.root.join("tmp", "exports")
        uploads_dir = Rails.root.join("tmp", "uploads")
        FileUtils.mkdir_p(exports_dir)
        FileUtils.mkdir_p(uploads_dir)

        export_file = exports_dir.join("export_old.zip")
        markdown_file = exports_dir.join("markdown_export_old.zip")
        import_file = uploads_dir.join("import_old.zip")

        past_time = 2.days.ago.to_time
        [ export_file, markdown_file, import_file ].each do |file|
          File.write(file, "data")
          File.utime(past_time, past_time, file)
        end

        result = Export.cleanup_old_exports(days: 1)

        assert_equal 3, result[:deleted]
        assert_equal 0, result[:errors]
        refute File.exist?(export_file)
        refute File.exist?(markdown_file)
        refute File.exist?(import_file)
        assert_match(/Cleaned up 3 old export\/import file/, result[:message])
      end
    end
  end

  test "exports users to csv" do
    exporter = Export.new
    exporter.send(:export_users)

    csv_path = File.join(exporter.export_dir, "users.csv")
    assert File.exist?(csv_path)
    content = File.read(csv_path)
    assert_includes content, "user_name"
    assert_includes content, "admin"
  ensure
    FileUtils.rm_rf(exporter.export_dir) if exporter&.export_dir
  end
end
