# frozen_string_literal: true

require "test_helper"

class JekyllRedirectsExporterTest < ActiveSupport::TestCase
  setup do
    @setting = JekyllSetting.instance
    @temp_dir = Dir.mktmpdir("jekyll_test")
    @setting.update!(
      jekyll_path: @temp_dir,
      repository_type: "local",
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )
    @redirect = Redirect.create!(
      regex: "/old-post",
      replacement: "/new-post",
      permanent: true
    )
    @exporter = JekyllRedirectsExporter.new(@setting)
  end

  teardown do
    FileUtils.rm_rf(@temp_dir) if @temp_dir && File.exist?(@temp_dir)
  end

  test "initializes with default setting" do
    exporter = JekyllRedirectsExporter.new
    assert_equal JekyllSetting.instance, exporter.setting
  end

  test "export_netlify generates correct format" do
    @setting.update!(redirect_export_format: "netlify")
    content = @exporter.export

    assert_includes content, "/old-post"
    assert_includes content, "/new-post"
    assert_includes content, "301"
  end

  test "export_netlify uses 302 for non-permanent redirects" do
    @redirect.update!(permanent: false)
    @setting.update!(redirect_export_format: "netlify")
    content = @exporter.export

    assert_includes content, "302"
  end

  test "export_vercel generates valid JSON" do
    @setting.update!(redirect_export_format: "vercel")
    content = @exporter.export

    parsed = JSON.parse(content)
    assert parsed.key?("redirects")
    assert_equal 1, parsed["redirects"].length
    assert_equal "/old-post", parsed["redirects"].first["source"]
    assert_equal "/new-post", parsed["redirects"].first["destination"]
    assert_equal true, parsed["redirects"].first["permanent"]
  end

  test "export_htaccess generates rewrite rules" do
    @setting.update!(redirect_export_format: "htaccess")
    content = @exporter.export

    assert_includes content, "RewriteEngine On"
    assert_includes content, "RewriteRule"
    assert_includes content, "[R=301,L]"
  end

  test "export_htaccess uses 302 for non-permanent" do
    @redirect.update!(permanent: false)
    @setting.update!(redirect_export_format: "htaccess")
    content = @exporter.export

    assert_includes content, "[R=302,L]"
  end

  test "export_nginx generates location blocks" do
    @setting.update!(redirect_export_format: "nginx")
    content = @exporter.export

    assert_includes content, "location ~"
    assert_includes content, "return 301 /new-post"
  end

  test "export_nginx uses 302 for non-permanent" do
    @redirect.update!(permanent: false)
    @setting.update!(redirect_export_format: "nginx")
    content = @exporter.export

    assert_includes content, "return 302"
  end

  test "export_jekyll_plugin generates YAML" do
    @setting.update!(redirect_export_format: "jekyll-plugin")
    content = @exporter.export

    parsed = YAML.safe_load(content)
    assert_equal 1, parsed.length
    assert_equal "/old-post", parsed.first["from"]
    assert_equal "/new-post", parsed.first["to"]
  end

  test "export_to_file creates netlify _redirects file" do
    @setting.update!(redirect_export_format: "netlify")
    filepath = @exporter.export_to_file

    assert_equal File.join(@temp_dir, "_redirects"), filepath
    assert File.exist?(filepath)
  end

  test "export_to_file creates vercel.json file" do
    @setting.update!(redirect_export_format: "vercel")
    filepath = @exporter.export_to_file

    assert_equal File.join(@temp_dir, "vercel.json"), filepath
    assert File.exist?(filepath)
  end

  test "export_to_file creates .htaccess file" do
    @setting.update!(redirect_export_format: "htaccess")
    filepath = @exporter.export_to_file

    assert_equal File.join(@temp_dir, ".htaccess"), filepath
    assert File.exist?(filepath)
  end

  test "export_to_file creates nginx config file" do
    @setting.update!(redirect_export_format: "nginx")
    filepath = @exporter.export_to_file

    assert_equal File.join(@temp_dir, "nginx_redirects.conf"), filepath
    assert File.exist?(filepath)
  end

  test "export_to_file creates jekyll plugin data file" do
    @setting.update!(redirect_export_format: "jekyll-plugin")
    filepath = @exporter.export_to_file

    assert_equal File.join(@temp_dir, "_data", "redirects.yml"), filepath
    assert File.exist?(filepath)
  end

  test "export_to_file returns nil when path invalid" do
    @setting.jekyll_path = "/nonexistent"
    result = @exporter.export_to_file

    assert_nil result
  end

  test "handles multiple redirects" do
    Redirect.create!(regex: "/another-old", replacement: "/another-new", permanent: false)

    @setting.update!(redirect_export_format: "netlify")
    content = @exporter.export

    assert_includes content, "/old-post"
    assert_includes content, "/another-old"
  end

  test "defaults to netlify format for unknown format" do
    @setting.redirect_export_format = "unknown"
    content = @exporter.export

    # Should produce netlify format
    assert_includes content, "301"
  end
end
