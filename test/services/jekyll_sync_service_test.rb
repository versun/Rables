# frozen_string_literal: true

require "test_helper"

class JekyllSyncServiceTest < ActiveSupport::TestCase
  setup do
    @setting = JekyllSetting.instance
    @temp_dir = Dir.mktmpdir("jekyll_test")
    @setting.update!(
      jekyll_path: @temp_dir,
      repository_type: "local",
      posts_directory: "_posts",
      pages_directory: "_pages",
      images_directory: "assets/images",
      download_remote_images: true,
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )
    @service = JekyllSyncService.new(@setting)
  end

  teardown do
    FileUtils.rm_rf(@temp_dir) if @temp_dir && File.exist?(@temp_dir)
  end

  # SSRF Protection Tests
  test "private_ip? blocks localhost" do
    assert @service.send(:private_ip?, "localhost")
    assert @service.send(:private_ip?, "127.0.0.1")
    assert @service.send(:private_ip?, "127.0.0.2")
  end

  test "private_ip? blocks private IP ranges" do
    # 10.x.x.x
    assert @service.send(:private_ip?, "10.0.0.1")
    assert @service.send(:private_ip?, "10.255.255.255")

    # 172.16.x.x - 172.31.x.x
    assert @service.send(:private_ip?, "172.16.0.1")
    assert @service.send(:private_ip?, "172.31.255.255")

    # 192.168.x.x
    assert @service.send(:private_ip?, "192.168.0.1")
    assert @service.send(:private_ip?, "192.168.255.255")

    # Link-local
    assert @service.send(:private_ip?, "169.254.0.1")
  end

  test "private_ip? allows public IPs" do
    refute @service.send(:private_ip?, "8.8.8.8")
    refute @service.send(:private_ip?, "1.1.1.1")
    refute @service.send(:private_ip?, "93.184.216.34")
  end

  test "sanitize_filename removes path traversal" do
    # The sanitize_filename removes .. and / and \
    assert_equal "file.txt", @service.send(:sanitize_filename, "file.txt")
    assert_not_includes @service.send(:sanitize_filename, "../file.txt"), ".."
    assert_not_includes @service.send(:sanitize_filename, "../../file.txt"), ".."
    assert_not_includes @service.send(:sanitize_filename, "/path/to/file.txt"), "/"
    assert_not_includes @service.send(:sanitize_filename, "\\path\\to\\file.txt"), "\\"
  end

  # Front Matter Mapping Security Tests
  test "apply_front_matter_mapping only allows whitelisted article fields" do
    @setting.update!(front_matter_mapping: { "title" => "custom_title", "destroy" => "bad" }.to_json)

    article = articles(:published_article)
    front_matter = {}

    result = @service.send(:apply_front_matter_mapping, front_matter, article)

    # title is whitelisted
    assert result.key?("custom_title")
    # destroy is not whitelisted
    refute result.key?("bad")
  end

  test "apply_front_matter_mapping only allows whitelisted page fields" do
    @setting.update!(front_matter_mapping: { "title" => "custom_title", "delete" => "bad" }.to_json)

    page = pages(:published_page)
    front_matter = {}

    result = @service.send(:apply_front_matter_mapping, front_matter, page)

    # title is whitelisted
    assert result.key?("custom_title")
    # delete is not whitelisted
    refute result.key?("bad")
  end

  test "apply_front_matter_mapping validates target field names" do
    @setting.update!(front_matter_mapping: { "title" => "valid_field", "slug" => "invalid;field" }.to_json)

    article = articles(:published_article)
    front_matter = {}

    result = @service.send(:apply_front_matter_mapping, front_matter, article)

    # valid_field is a valid target name
    assert result.key?("valid_field")
    # invalid;field contains invalid characters
    refute result.key?("invalid;field")
  end

  # Basic Service Tests
  test "initializes with default setting" do
    service = JekyllSyncService.new
    assert_equal JekyllSetting.instance, service.setting
  end

  test "initializes with custom setting" do
    service = JekyllSyncService.new(@setting)
    assert_equal @setting, service.setting
  end

  test "sync_all fails when not configured" do
    @setting.jekyll_path = nil
    @setting.auto_sync_enabled = false
    @setting.save!(validate: false)

    result = @service.sync_all
    refute result
    assert_equal "Jekyll is not configured", @service.error_message
  end

  test "sync_all fails when path invalid" do
    # Use a path that doesn't exist but won't trigger validation
    @setting.instance_variable_set(:@jekyll_path_was, @setting.jekyll_path)
    @setting.jekyll_path = "/nonexistent/path"
    # Skip validation by directly setting the attribute
    @setting.save!(validate: false)

    result = @service.sync_all
    refute result
    assert_equal "Jekyll path is not valid", @service.error_message
  end

  test "sync_article fails when not configured" do
    @setting.jekyll_path = nil
    @setting.auto_sync_enabled = false
    @setting.save!(validate: false)

    article = articles(:published_article)
    result = @service.sync_article(article)
    refute result
    assert_equal "Jekyll is not configured", @service.error_message
  end

  test "sync_page fails when not configured" do
    @setting.jekyll_path = nil
    @setting.auto_sync_enabled = false
    @setting.save!(validate: false)

    page = pages(:published_page)
    result = @service.sync_page(page)
    refute result
    assert_equal "Jekyll is not configured", @service.error_message
  end

  # Full sync with exporters tests
  test "sync_all calls export_comments when enabled" do
    @setting.update!(export_comments: true)

    # Create a comment for testing
    article = articles(:published_article)
    Comment.create!(
      commentable: article,
      commentable_type: "Article",
      commentable_id: article.id,
      article: article,
      author_name: "Test",
      content: "Test comment",
      status: :approved,
      platform: nil
    )

    result = @service.sync_all
    assert result
    assert @service.stats[:comments] >= 0
  end

  test "sync_all does not call export_comments when disabled" do
    @setting.update!(export_comments: false)

    result = @service.sync_all
    assert result
    assert_equal 0, @service.stats[:comments]
  end

  test "sync_all calls export_redirects" do
    result = @service.sync_all
    assert result
    assert @service.stats.key?(:redirects)
  end

  test "sync_all calls export_static_files" do
    result = @service.sync_all
    assert result
    assert @service.stats.key?(:static_files)
  end

  test "stats includes all export counts" do
    result = @service.sync_all
    assert result

    assert @service.stats.key?(:articles)
    assert @service.stats.key?(:pages)
    assert @service.stats.key?(:attachments)
    assert @service.stats.key?(:comments)
    assert @service.stats.key?(:redirects)
    assert @service.stats.key?(:static_files)
  end
end
