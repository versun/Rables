# frozen_string_literal: true

require "test_helper"

class JekyllSettingTest < ActiveSupport::TestCase
  setup do
    @setting = JekyllSetting.instance
    @temp_dir = Dir.mktmpdir("jekyll_test")
  end

  teardown do
    FileUtils.rm_rf(@temp_dir) if @temp_dir && File.exist?(@temp_dir)
  end

  test "singleton pattern returns same instance" do
    setting1 = JekyllSetting.instance
    setting2 = JekyllSetting.instance
    assert_equal setting1.id, setting2.id
  end

  test "validates repository_type inclusion" do
    @setting.repository_type = "local"
    @setting.jekyll_path = @temp_dir
    assert @setting.valid?

    @setting.repository_type = "git"
    @setting.repository_url = "https://github.com/test/repo.git"
    assert @setting.valid?

    @setting.repository_type = "invalid"
    assert_not @setting.valid?
    assert_includes @setting.errors[:repository_type], "is not included in the list"
  end

  test "validates redirect_export_format inclusion" do
    @setting.jekyll_path = @temp_dir
    %w[netlify vercel htaccess nginx jekyll-plugin].each do |format|
      @setting.redirect_export_format = format
      assert @setting.valid?, "Expected #{format} to be valid"
    end

    @setting.redirect_export_format = "invalid"
    assert_not @setting.valid?
  end

  test "validates comments_format inclusion" do
    @setting.jekyll_path = @temp_dir
    %w[yaml json].each do |format|
      @setting.comments_format = format
      assert @setting.valid?, "Expected #{format} to be valid"
    end

    @setting.comments_format = "invalid"
    assert_not @setting.valid?
  end

  test "validates jekyll_path presence when auto_sync_enabled" do
    @setting.auto_sync_enabled = true
    @setting.jekyll_path = nil
    assert_not @setting.valid?
    assert_includes @setting.errors[:jekyll_path], "can't be blank"
  end

  test "validates repository_url presence when git repository" do
    @setting.repository_type = "git"
    @setting.repository_url = nil
    assert_not @setting.valid?
    assert_includes @setting.errors[:repository_url], "can't be blank"
  end

  test "validates jekyll_path is writable directory" do
    @setting.jekyll_path = @temp_dir
    assert @setting.valid?

    @setting.jekyll_path = "/nonexistent/path"
    assert_not @setting.valid?
    assert_includes @setting.errors[:jekyll_path], "does not exist or is not a directory"
  end

  test "validates jekyll_path rejects path traversal" do
    @setting.jekyll_path = "/tmp/../etc/passwd"
    assert_not @setting.valid?
    assert_includes @setting.errors[:jekyll_path], "contains invalid characters"

    @setting.jekyll_path = "~/Documents"
    assert_not @setting.valid?
    assert_includes @setting.errors[:jekyll_path], "contains invalid characters"
  end

  test "validates front_matter_mapping is valid JSON" do
    @setting.jekyll_path = @temp_dir
    @setting.front_matter_mapping = '{"layout": "post"}'
    assert @setting.valid?

    @setting.front_matter_mapping = "{invalid json"
    assert_not @setting.valid?
    assert_includes @setting.errors[:front_matter_mapping], "must be valid JSON"
  end

  test "front_matter_mapping_hash parses JSON correctly" do
    @setting.front_matter_mapping = '{"layout": "post", "author": "admin"}'
    expected = { "layout" => "post", "author" => "admin" }
    assert_equal expected, @setting.front_matter_mapping_hash
  end

  test "front_matter_mapping_hash returns empty hash for invalid JSON" do
    @setting.front_matter_mapping = "{invalid"
    assert_equal({}, @setting.front_matter_mapping_hash)
  end

  test "front_matter_mapping_hash= sets JSON string" do
    @setting.front_matter_mapping_hash = { "layout" => "page" }
    assert_equal '{"layout":"page"}', @setting.front_matter_mapping
  end

  test "repository type helpers" do
    @setting.repository_type = "git"
    assert @setting.git_repository?
    assert_not @setting.local_repository?

    @setting.repository_type = "local"
    assert @setting.local_repository?
    assert_not @setting.git_repository?
  end

  test "full path helpers return correct paths" do
    @setting.jekyll_path = "/home/user/blog"
    @setting.posts_directory = "_posts"
    @setting.pages_directory = "_pages"
    @setting.assets_directory = "assets/images"
    @setting.images_directory = "assets/images/posts"
    @setting.static_files_directory = "assets"

    assert_equal "/home/user/blog/_posts", @setting.full_posts_path
    assert_equal "/home/user/blog/_pages", @setting.full_pages_path
    assert_equal "/home/user/blog/assets/images", @setting.full_assets_path
    assert_equal "/home/user/blog/assets/images/posts", @setting.full_images_path
    assert_equal "/home/user/blog/assets", @setting.full_static_files_path
    assert_equal "/home/user/blog/_data/comments", @setting.comments_data_path
  end

  test "full path helpers return nil when jekyll_path is blank" do
    @setting.jekyll_path = nil

    assert_nil @setting.full_posts_path
    assert_nil @setting.full_pages_path
    assert_nil @setting.full_assets_path
    assert_nil @setting.full_images_path
    assert_nil @setting.full_static_files_path
    assert_nil @setting.comments_data_path
  end

  test "configured? returns true when properly configured" do
    @setting.jekyll_path = @temp_dir
    @setting.repository_type = "local"
    assert @setting.configured?

    @setting.repository_type = "git"
    @setting.repository_url = "https://github.com/user/repo.git"
    assert @setting.configured?
  end

  test "configured? returns false when not properly configured" do
    @setting.jekyll_path = nil
    assert_not @setting.configured?

    @setting.jekyll_path = @temp_dir
    @setting.repository_type = "git"
    @setting.repository_url = nil
    assert_not @setting.configured?
  end

  test "ready_for_sync? checks both configured and path valid" do
    @setting.jekyll_path = @temp_dir
    @setting.repository_type = "local"
    assert @setting.ready_for_sync?

    @setting.jekyll_path = "/nonexistent"
    assert_not @setting.ready_for_sync?
  end

  test "jekyll_path_valid? checks directory exists and is writable" do
    @setting.jekyll_path = @temp_dir
    assert @setting.jekyll_path_valid?

    @setting.jekyll_path = "/nonexistent"
    assert_not @setting.jekyll_path_valid?

    @setting.jekyll_path = nil
    assert_not @setting.jekyll_path_valid?
  end

  test "normalize_paths strips and removes trailing slashes" do
    # Create a temp dir with trailing slash in the path we'll test
    @setting.jekyll_path = @temp_dir
    @setting.repository_url = "  https://github.com/user/repo.git  "
    @setting.save!

    assert_equal @temp_dir, @setting.jekyll_path
    assert_equal "https://github.com/user/repo.git", @setting.repository_url
  end

  test "normalize_paths sets default directories" do
    @setting.jekyll_path = @temp_dir
    @setting.posts_directory = nil
    @setting.pages_directory = nil
    @setting.assets_directory = nil
    @setting.images_directory = nil
    @setting.static_files_directory = nil
    @setting.save!

    assert_equal "_posts", @setting.posts_directory
    assert_equal "_pages", @setting.pages_directory
    assert_equal "assets/images", @setting.assets_directory
    assert_equal "assets/images/posts", @setting.images_directory
    assert_equal "assets", @setting.static_files_directory
  end

  # Branch name validation tests (security)
  test "validates branch name format" do
    @setting.jekyll_path = @temp_dir
    @setting.repository_type = "local"

    # Valid branch names
    @setting.branch = "main"
    assert @setting.valid?

    @setting.branch = "feature/new-feature"
    assert @setting.valid?

    @setting.branch = "release-1.0.0"
    assert @setting.valid?

    @setting.branch = "hotfix_123"
    assert @setting.valid?
  end

  test "rejects branch names with command injection characters" do
    @setting.jekyll_path = @temp_dir
    @setting.repository_type = "local"

    # Invalid branch names (command injection attempts)
    @setting.branch = "main; rm -rf /"
    assert_not @setting.valid?
    assert_includes @setting.errors[:branch], "contains invalid characters"

    @setting.branch = "main && cat /etc/passwd"
    assert_not @setting.valid?

    @setting.branch = "main | cat /etc/passwd"
    assert_not @setting.valid?

    @setting.branch = "main`whoami`"
    assert_not @setting.valid?

    @setting.branch = "$(whoami)"
    assert_not @setting.valid?
  end

  test "rejects branch names starting with dash" do
    @setting.jekyll_path = @temp_dir
    @setting.repository_type = "local"

    @setting.branch = "-dangerous"
    assert_not @setting.valid?
    assert_includes @setting.errors[:branch], "contains invalid characters"
  end

  test "rejects branch names with path traversal" do
    @setting.jekyll_path = @temp_dir
    @setting.repository_type = "local"

    @setting.branch = "feature/../main"
    assert_not @setting.valid?
    assert_includes @setting.errors[:branch], "contains invalid characters"
  end
end
