# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class ImportZipTest < ActiveSupport::TestCase
  test "imports article source reference and seo fields" do
    slug = "import-seo-source-#{SecureRandom.hex(6)}"
    csv_content = CSV.generate(
      write_headers: true,
      headers: %w[
        id title slug description content status scheduled_at
        source_url source_author source_content
        meta_title meta_description meta_image
        created_at updated_at
      ]
    ) do |csv|
      csv << [
        1,
        "SEO + Source",
        slug,
        "desc",
        "<p>Hello</p>",
        "publish",
        nil,
        "https://example.com/source",
        "Example Author",
        "Example quote",
        "Example Meta Title",
        "Example Meta Description",
        "https://example.com/meta.jpg",
        Time.current,
        Time.current
      ]
    end

    zip_path = build_zip("articles.csv" => csv_content)

    importer = ImportZip.new(zip_path)

    assert importer.import_data, importer.error_message

    article = Article.find_by!(slug: slug)
    assert_equal "https://example.com/source", article.source_url
    assert_equal "Example Author", article.source_author
    assert_equal "Example quote", article.source_content
    assert_equal "Example Meta Title", article.meta_title
    assert_equal "Example Meta Description", article.meta_description
    assert_equal "https://example.com/meta.jpg", article.meta_image
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports an image-only article without failing validation" do
    csv_content = CSV.generate(write_headers: true, headers: %w[id title slug description content status scheduled_at created_at updated_at]) do |csv|
      csv << [
        1,
        "Image Only",
        "image-only",
        "",
        "<p><img src=\"attachments/a.png\" alt=\"A\"></p>",
        "publish",
        nil,
        Time.current,
        Time.current
      ]
    end

    zip_path = build_zip(
      "articles.csv" => csv_content,
      "attachments/a.png" => "not-a-real-png"
    )

    importer = ImportZip.new(zip_path)

    assert importer.import_data, importer.error_message

    article = Article.find_by(slug: "image-only")
    assert article, "expected article to be imported"
    assert article.html?, "expected importer to fall back to html content type"
    assert_includes article.html_content, "<img"
    assert_includes article.html_content, "/rails/active_storage/blobs/"
    refute_includes article.html_content, "attachments/a.png"
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports an image-only page without failing validation" do
    csv_content = CSV.generate(write_headers: true, headers: %w[id title slug content status redirect_url page_order created_at updated_at]) do |csv|
      csv << [
        1,
        "Image Only Page",
        "image-only-page",
        "<p><img src=\"attachments/p.png\" alt=\"P\"></p>",
        "publish",
        "",
        0,
        Time.current,
        Time.current
      ]
    end

    zip_path = build_zip(
      "pages.csv" => csv_content,
      "attachments/p.png" => "not-a-real-png"
    )

    importer = ImportZip.new(zip_path)

    assert importer.import_data, importer.error_message

    page = Page.find_by(slug: "image-only-page")
    assert page, "expected page to be imported"
    assert page.html?, "expected importer to fall back to html content type"
    assert_includes page.html_content, "<img"
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports comments with commentable set" do
    articles_csv = CSV.generate(write_headers: true, headers: %w[id title slug description content status scheduled_at created_at updated_at]) do |csv|
      csv << [
        1,
        "Image Only",
        "image-only",
        "",
        "<p><img src=\"attachments/a.png\" alt=\"A\"></p>",
        "publish",
        nil,
        Time.current,
        Time.current
      ]
    end

    comments_csv = CSV.generate(
      write_headers: true,
      headers: %w[id article_id article_slug parent_id author_name author_url author_username author_avatar_url content platform external_id status published_at url created_at updated_at]
    ) do |csv|
      csv << [
        1,
        "",
        "image-only",
        "",
        "Alice",
        "",
        "",
        "",
        "Nice post",
        "",
        "",
        "approved",
        "",
        "",
        Time.current,
        Time.current
      ]
    end

    zip_path = build_zip(
      "articles.csv" => articles_csv,
      "comments.csv" => comments_csv,
      "attachments/a.png" => "not-a-real-png"
    )

    importer = ImportZip.new(zip_path)

    assert importer.import_data, importer.error_message

    article = Article.find_by!(slug: "image-only")
    comment = Comment.find_by!(author_name: "Alice", content: "Nice post")

    assert_equal "Article", comment.commentable_type
    assert_equal article.id, comment.commentable_id
    assert_equal article.id, comment.article_id
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports crossposts by platform" do
    Crosspost.delete_all

    crossposts_csv = CSV.generate(
      write_headers: true,
      headers: %w[id platform server_url client_key client_secret access_token access_token_secret api_key api_key_secret username app_password enabled created_at updated_at]
    ) do |csv|
      csv << [
        1,
        "mastodon",
        "https://mastodon.social",
        "test_client_key",
        "test_client_secret",
        "test_access_token",
        "",
        "",
        "",
        "",
        "",
        true,
        Time.current,
        Time.current
      ]
      csv << [
        2,
        "twitter",
        "",
        "",
        "",
        "twitter_access_token",
        "twitter_access_token_secret",
        "twitter_api_key",
        "twitter_api_key_secret",
        "",
        "",
        true,
        Time.current,
        Time.current
      ]
    end

    zip_path = build_zip("crossposts.csv" => crossposts_csv)

    importer = ImportZip.new(zip_path)

    assert importer.import_data, importer.error_message
    assert_equal 2, Crosspost.count

    mastodon = Crosspost.find_by!(platform: "mastodon")
    assert mastodon.enabled?
    assert_equal "test_client_key", mastodon.client_key
    assert_equal "test_client_secret", mastodon.client_secret
    assert_equal "test_access_token", mastodon.access_token

    twitter = Crosspost.find_by!(platform: "twitter")
    assert twitter.enabled?
    assert_equal "twitter_access_token", twitter.access_token
    assert_equal "twitter_access_token_secret", twitter.access_token_secret
    assert_equal "twitter_api_key", twitter.api_key
    assert_equal "twitter_api_key_secret", twitter.api_key_secret
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports git integrations by provider" do
    git_integrations_csv = CSV.generate(
      write_headers: true,
      headers: %w[id provider name server_url username access_token enabled created_at updated_at]
    ) do |csv|
      csv << [
        1,
        "github",
        "GitHub Updated",
        "https://github.com",
        "",
        "ghp_new_token_67890",
        true,
        Time.current,
        Time.current
      ]
    end

    zip_path = build_zip("git_integrations.csv" => git_integrations_csv)

    importer = ImportZip.new(zip_path)

    assert importer.import_data, importer.error_message

    git_integration = GitIntegration.find_by!(provider: "github")
    assert_equal "GitHub Updated", git_integration.name
    assert_equal "https://github.com", git_integration.server_url
    assert_equal "ghp_new_token_67890", git_integration.access_token
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports redirects with numeric enabled and permanent flags" do
    redirect_slug = "import-redirect-#{SecureRandom.hex(4)}"
    redirects_csv = CSV.generate(
      write_headers: true,
      headers: %w[id regex replacement enabled permanent created_at updated_at]
    ) do |csv|
      csv << [
        1,
        "^/#{redirect_slug}$",
        "/#{redirect_slug}-target",
        "0",
        "1",
        Time.current,
        Time.current
      ]
    end

    zip_path = build_zip("redirects.csv" => redirects_csv)

    importer = ImportZip.new(zip_path)

    assert importer.import_data, importer.error_message

    redirect = Redirect.find_by!(regex: "^/#{redirect_slug}$")
    refute redirect.enabled?, "expected redirect to remain disabled when enabled is 0"
    assert redirect.permanent?, "expected redirect to be marked permanent when permanent is 1"
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports a full dataset with attachments" do
    ArticleTag.delete_all
    SubscriberTag.delete_all
    SocialMediaPost.delete_all
    Comment.delete_all
    Tag.delete_all
    Article.delete_all
    Subscriber.delete_all
    Page.delete_all
    StaticFile.delete_all
    Redirect.delete_all
    Setting.delete_all
    NewsletterSetting.delete_all
    Crosspost.delete_all
    GitIntegration.delete_all
    Listmonk.delete_all

    tag_slug = "tag-#{SecureRandom.hex(4)}"
    article_slug = "article-#{SecureRandom.hex(4)}"
    page_slug = "page-#{SecureRandom.hex(4)}"
    subscriber_email = "subscriber-#{SecureRandom.hex(4)}@example.com"

    attachment_json = {
      "contentType" => "image/png",
      "url" => "attachments/article_1/figure.png",
      "filename" => "figure.png"
    }.to_json

    article_content = <<~HTML.squish
      <p>Hello</p>
      <action-text-attachment content-type="image/png" url="attachments/article_1/inline.png" filename="inline.png"></action-text-attachment>
      <figure data-trix-attachment='#{attachment_json}'></figure>
      <img src="attachments/article_1/image.png">
    HTML

    tags_csv = CSV.generate(write_headers: true, headers: %w[id name slug created_at updated_at]) do |csv|
      csv << [ 1, "Tag", tag_slug, Time.current, Time.current ]
    end

    articles_csv = CSV.generate(write_headers: true, headers: %w[
      id title slug description content status scheduled_at
      source_url source_author source_content
      meta_title meta_description meta_image
      content_type html_content comment
      created_at updated_at
    ]) do |csv|
      csv << [
        1,
        "Imported Article",
        article_slug,
        "desc",
        article_content,
        "publish",
        nil,
        "",
        "",
        "",
        "",
        "",
        "",
        "rich_text",
        "",
        "false",
        Time.current,
        Time.current
      ]
    end

    article_tags_csv = CSV.generate(write_headers: true, headers: %w[id article_slug tag_slug created_at updated_at]) do |csv|
      csv << [ 1, article_slug, tag_slug, Time.current, Time.current ]
    end

    crossposts_csv = CSV.generate(
      write_headers: true,
      headers: %w[id platform server_url client_key client_secret access_token access_token_secret api_key api_key_secret username app_password enabled auto_fetch_comments comment_fetch_schedule max_characters settings created_at updated_at]
    ) do |csv|
      csv << [
        1,
        "mastodon",
        "https://mastodon.example.com",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "",
        "true",
        "false",
        "",
        "",
        "{}",
        Time.current,
        Time.current
      ]
    end

    listmonks_csv = CSV.generate(write_headers: true, headers: %w[id url username api_key list_id template_id enabled created_at updated_at]) do |csv|
      csv << [ 1, "https://listmonk.example.com", "user", "key", 1, 2, "true", Time.current, Time.current ]
    end

    git_integrations_csv = CSV.generate(write_headers: true, headers: %w[id provider name server_url username access_token enabled created_at updated_at]) do |csv|
      csv << [ 1, "github", "GitHub", "https://github.com", "user", "", "true", Time.current, Time.current ]
    end

    pages_csv = CSV.generate(write_headers: true, headers: %w[id title slug content status redirect_url page_order content_type html_content created_at updated_at]) do |csv|
      csv << [
        1,
        "Imported Page",
        page_slug,
        "<p>Page</p><img src=\"attachments/page_1/image.png\">",
        "publish",
        "",
        1,
        "html",
        "",
        Time.current,
        Time.current
      ]
    end

    settings_csv = CSV.generate(write_headers: true, headers: %w[
      id title description author url time_zone giscus tool_code head_code custom_css
      social_links static_files auto_regenerate_triggers deploy_branch deploy_provider deploy_repo_url
      local_generation_path static_generation_destination static_generation_delay setup_completed
      github_backup_enabled github_repo_url github_token github_backup_branch created_at updated_at
    ]) do |csv|
      csv << [
        1,
        "Imported Site",
        "desc",
        "Author",
        "http://example.com",
        "UTC",
        "",
        "",
        "",
        "",
        "{invalid",
        "{}",
        "{invalid",
        "main",
        "",
        "",
        "",
        "local",
        "",
        "true",
        "false",
        "",
        "",
        "main",
        Time.current,
        Time.current
      ]
    end

    setting_footers_csv = CSV.generate(write_headers: true, headers: %w[content]) do |csv|
      csv << [ "<p>Footer</p><img src=\"attachments/setting_1/footer.png\">" ]
    end

    social_media_posts_csv = CSV.generate(write_headers: true, headers: %w[id article_slug platform url created_at updated_at]) do |csv|
      csv << [ 1, article_slug, "twitter", "https://example.com/post", Time.current, Time.current ]
    end

    comments_csv = CSV.generate(
      write_headers: true,
      headers: %w[id article_id article_slug parent_id author_name author_url author_username author_avatar_url content platform external_id status published_at url created_at updated_at]
    ) do |csv|
      csv << [ 1, "", article_slug, "", "Parent", "", "", "", "Parent comment", "", "", "approved", "", "", Time.current, Time.current ]
      csv << [ 2, "", article_slug, 1, "Child", "", "", "", "Child comment", "twitter", "ext-1", "approved", "", "", Time.current, Time.current ]
    end

    static_files_csv = CSV.generate(write_headers: true, headers: %w[id filename description blob_filename created_at updated_at]) do |csv|
      csv << [ 1, "static.txt", "Static file", "static.txt", Time.current, Time.current ]
    end

    redirects_csv = CSV.generate(write_headers: true, headers: %w[id regex replacement enabled permanent created_at updated_at]) do |csv|
      csv << [ 1, "^/imported$", "/target", "true", "false", Time.current, Time.current ]
    end

    newsletter_settings_csv = CSV.generate(
      write_headers: true,
      headers: %w[id provider enabled smtp_address smtp_port smtp_user_name smtp_password smtp_domain smtp_authentication smtp_enable_starttls from_email footer created_at updated_at]
    ) do |csv|
      csv << [
        1,
        "native",
        "true",
        "smtp.example.com",
        587,
        "user",
        "pass",
        "",
        "plain",
        "true",
        "from@example.com",
        "<p>Newsletter</p><img src=\"attachments/newsletter_setting_1/footer.png\">",
        Time.current,
        Time.current
      ]
    end

    subscribers_csv = CSV.generate(write_headers: true, headers: %w[id email confirmation_token confirmed_at unsubscribe_token unsubscribed_at created_at updated_at]) do |csv|
      csv << [ 1, subscriber_email, "token", Time.current, "unsub", "", Time.current, Time.current ]
    end

    subscriber_tags_csv = CSV.generate(write_headers: true, headers: %w[id subscriber_email tag_slug created_at updated_at]) do |csv|
      csv << [ 1, subscriber_email, tag_slug, Time.current, Time.current ]
    end

    zip_path = build_zip(
      "tags.csv" => tags_csv,
      "articles.csv" => articles_csv,
      "article_tags.csv" => article_tags_csv,
      "crossposts.csv" => crossposts_csv,
      "listmonks.csv" => listmonks_csv,
      "git_integrations.csv" => git_integrations_csv,
      "pages.csv" => pages_csv,
      "settings.csv" => settings_csv,
      "setting_footers.csv" => setting_footers_csv,
      "social_media_posts.csv" => social_media_posts_csv,
      "comments.csv" => comments_csv,
      "static_files.csv" => static_files_csv,
      "redirects.csv" => redirects_csv,
      "newsletter_settings.csv" => newsletter_settings_csv,
      "subscribers.csv" => subscribers_csv,
      "subscriber_tags.csv" => subscriber_tags_csv,
      "attachments/article_1/inline.png" => "inline",
      "attachments/article_1/figure.png" => "figure",
      "attachments/article_1/image.png" => "image",
      "attachments/page_1/image.png" => "page",
      "attachments/setting_1/footer.png" => "footer",
      "attachments/newsletter_setting_1/footer.png" => "newsletter",
      "attachments/static_files/1_static.txt" => "static"
    )

    importer = ImportZip.new(zip_path)

    assert importer.import_data, importer.error_message
    assert Article.find_by!(slug: article_slug)
    assert Page.find_by!(slug: page_slug)
    assert Tag.find_by!(slug: tag_slug)
    assert ArticleTag.find_by!(tag_id: Tag.find_by!(slug: tag_slug).id)
    assert SocialMediaPost.find_by!(platform: "twitter")
    assert Comment.find_by!(author_name: "Child")
    assert StaticFile.find_by!(filename: "static.txt")
    assert Redirect.find_by!(regex: "^/imported$")
    assert NewsletterSetting.first
    assert Subscriber.find_by!(email: subscriber_email)
    assert SubscriberTag.find_by!(subscriber_id: Subscriber.find_by!(email: subscriber_email).id)

    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("blob"),
      filename: "blob.png",
      content_type: "image/png"
    )
    blob_url = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)
    assert_equal blob, importer.send(:extract_blob_from_url, blob_url)
    assert_equal ".png", importer.send(:extract_extension_from_url, "http://example.com/image.png")
    assert_nil importer.send(:extract_extension_from_url, "http://example.com/noext")

    head_response = Net::HTTPOK.new("1.1", "200", "OK")
    head_response["Content-Type"] = "image/png"
    head_response.instance_variable_set(:@read, true)
    http = Struct.new(:response) { def head(_url) response end }.new(head_response)

    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &block) { block.call(http) }) do
      assert_equal "image/png", importer.send(:detect_content_type_from_url, "http://example.com/image.png")
    end

    img_node = Nokogiri::HTML.fragment("<img src=\"http://example.com/remote.png\">").at_css("img")
    uri_stub = lambda do |_url, **_kwargs, &block|
      io = StringIO.new("remote")
      block ? block.call(io) : io
    end

    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &block) { block.call(http) }) do
      URI.stub(:open, uri_stub) do
        importer.send(:download_and_process_remote_image, img_node, "http://example.com/remote.png", "rec", "type")
        assert_includes img_node["src"], "/rails/active_storage/blobs/"
      end
    end

    importer.send(:safe_process) { raise "boom" }

    Comment.where.not(platform: nil).delete_all
    second_importer = ImportZip.new(zip_path)
    assert second_importer.import_data, second_importer.error_message

    attachment_node = Nokogiri::HTML.fragment(
      "<action-text-attachment content-type=\"image/png\" url=\"#{blob_url}\" filename=\"#{blob.filename}\"></action-text-attachment>"
    ).at_css("action-text-attachment")
    importer.send(:process_imported_attachment_element, attachment_node, "rec", "attachment")
    assert_includes attachment_node["url"], "/rails/active_storage/blobs/"

    figure_json = {
      "contentType" => "image/png",
      "url" => blob_url,
      "filename" => blob.filename.to_s
    }.to_json
    figure_node = Nokogiri::HTML.fragment("<figure data-trix-attachment='#{figure_json}'></figure>").at_css("figure")
    importer.send(:process_imported_figure_element, figure_node, "rec", "figure")
    assert_includes figure_node["data-trix-attachment"], "/rails/active_storage/blobs/"

    img_node = Nokogiri::HTML.fragment("<img src=\"#{blob_url}\">").at_css("img")
    importer.send(:process_imported_image_element, img_node, "rec", "image")
    assert_includes img_node["src"], "/rails/active_storage/blobs/"

    fix_content = "<action-text-attachment filename=\"#{blob.filename}\" sgid=\"bad\" url=\"#{blob_url}\"></action-text-attachment>"
    fixed = importer.send(:fix_content_sgid_references, fix_content)
    refute_includes fixed, 'sgid="bad"'
    assert_includes fixed, "/rails/active_storage/blobs/redirect/"

    missing_attachment = Nokogiri::HTML.fragment(
      "<action-text-attachment content-type=\"image/png\" url=\"attachments/missing.png\" filename=\"missing.png\"></action-text-attachment>"
    ).at_css("action-text-attachment")
    importer.send(:process_imported_attachment_element, missing_attachment, "rec", "attachment")

    missing_figure_json = { "contentType" => "image/png", "url" => "attachments/missing.png", "filename" => "missing.png" }.to_json
    missing_figure = Nokogiri::HTML.fragment("<figure data-trix-attachment='#{missing_figure_json}'></figure>").at_css("figure")
    importer.send(:process_imported_figure_element, missing_figure, "rec", "figure")

    missing_img = Nokogiri::HTML.fragment("<img src=\"attachments/missing.png\">").at_css("img")
    importer.send(:process_imported_image_element, missing_img, "rec", "image")

    assert_nil importer.send(:extract_blob_from_url, "/rails/active_storage/blobs/redirect/bad-signed-id")

    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &_block) { raise "boom" }) do
      assert_equal "image/jpeg", importer.send(:detect_content_type_from_url, "http://example.com/image.png")
    end

    assert importer.send(:safe_file_path?, importer.import_dir.join("ok.txt"))
    refute importer.send(:safe_file_path?, Rails.root.join("tmp/outside.txt"))
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "helper utilities handle json, booleans, and path checks" do
    zip_path = build_zip("tags.csv" => "id,name\n")
    importer = ImportZip.new(zip_path)

    assert_nil importer.send(:parse_json_field, "")
    assert_equal({}, importer.send(:parse_json_field, "{invalid"))
    assert_nil importer.send(:parse_json_field, "null")

    assert_equal true, importer.send(:cast_boolean, "true", default: false)
    assert_equal false, importer.send(:cast_boolean, nil, default: false)
    assert_equal true, importer.send(:cast_boolean, nil, default: true)

    assert importer.send(:is_local_attachment?, "attachments/file.png")
    refute importer.send(:is_local_attachment?, "http://example.com/file.png")
    assert importer.send(:is_active_storage_url?, "/rails/active_storage/blobs/redirect/abc")

    other_base = Rails.root.join("tmp")
    assert_equal File.join(other_base, "path.txt"), importer.send(:safe_join_path, other_base, "path.txt")
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
    FileUtils.rm_rf(importer.import_dir) if importer&.import_dir
  end

  private

  def build_zip(files)
    require "zip"

    zip_path = Rails.root.join("tmp", "import_zip_test_#{SecureRandom.hex(6)}.zip")

    Zip::File.open(zip_path, create: true) do |zip|
      files.each do |name, content|
        zip.get_output_stream(name) { |f| f.write(content) }
      end
    end

    zip_path
  end
end
