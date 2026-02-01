# frozen_string_literal: true

require "test_helper"

class JekyllSettingTest < ActiveSupport::TestCase
  def setup
    @setting = JekyllSetting.instance
    @setting.update(
      jekyll_path: "/tmp/test_jekyll_site",
      repository_type: "local",
      auto_sync_enabled: false
    )
  end

  test "instance returns first or creates new" do
    assert_instance_of JekyllSetting, JekyllSetting.instance
  end

  test "validates repository_type inclusion" do
    @setting.repository_type = "invalid"
    assert_not @setting.valid?
    assert_includes @setting.errors[:repository_type], "is not included in the list"
  end

  test "validates redirect_export_format inclusion" do
    @setting.redirect_export_format = "invalid"
    assert_not @setting.valid?
    assert_includes @setting.errors[:redirect_export_format], "is not included in the list"
  end

  test "validates comments_format inclusion" do
    @setting.comments_format = "invalid"
    assert_not @setting.valid?
    assert_includes @setting.errors[:comments_format], "is not included in the list"
  end

  test "validates front_matter_mapping is valid JSON" do
    @setting.front_matter_mapping = "invalid json"
    assert_not @setting.valid?
    assert_includes @setting.errors[:front_matter_mapping], "must be valid JSON"
  end

  test "front_matter_mapping_hash returns parsed JSON" do
    @setting.front_matter_mapping = '{"custom_key": "custom_value"}'
    assert_equal({"custom_key" => "custom_value"}, @setting.front_matter_mapping_hash)
  end

  test "front_matter_mapping_hash returns empty hash for blank" do
    @setting.front_matter_mapping = nil
    assert_equal({}, @setting.front_matter_mapping_hash)
  end

  test "git_enabled? returns true for git repository type with url" do
    @setting.repository_type = "git"
    @setting.repository_url = "https://github.com/user/repo.git"
    assert @setting.git_enabled?
  end

  test "git_enabled? returns false for local repository type" do
    @setting.repository_type = "local"
    assert_not @setting.git_enabled?
  end

  test "git_enabled? returns false without repository_url" do
    @setting.repository_type = "git"
    @setting.repository_url = nil
    assert_not @setting.git_enabled?
  end

  test "posts_full_path returns correct path" do
    FileUtils.mkdir_p(@setting.jekyll_path)
    expected = File.join(@setting.jekyll_path, @setting.posts_directory)
    assert_equal expected, @setting.posts_full_path
  end

  test "pages_full_path returns correct path" do
    FileUtils.mkdir_p(@setting.jekyll_path)
    expected = File.join(@setting.jekyll_path, @setting.pages_directory)
    assert_equal expected, @setting.pages_full_path
  end

  test "images_full_path returns correct path" do
    FileUtils.mkdir_p(@setting.jekyll_path)
    expected = File.join(@setting.jekyll_path, @setting.images_directory)
    assert_equal expected, @setting.images_full_path
  end
end
