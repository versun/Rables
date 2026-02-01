# frozen_string_literal: true

require "test_helper"

class JekyllSettingTest < ActiveSupport::TestCase
  test "instance applies defaults" do
    setting = JekyllSetting.instance

    assert_equal "local", setting.repository_type
    assert_equal "_posts", setting.posts_directory
    assert_equal "_pages", setting.pages_directory
    assert_equal "assets/images/posts", setting.images_directory
    assert_equal true, setting.export_comments
  end

  test "rejects relative jekyll_path" do
    setting = JekyllSetting.new(jekyll_path: "relative/path")

    refute setting.valid?
    assert_includes setting.errors[:jekyll_path], "必须是绝对路径"
  end

  test "requires repository_url when repository_type is git" do
    dir = Dir.mktmpdir
    setting = JekyllSetting.new(jekyll_path: dir, repository_type: "git")

    refute setting.valid?
    assert setting.errors[:repository_url].any?
  ensure
    FileUtils.remove_entry(dir) if dir
  end

  test "parses front_matter_mapping_json" do
    dir = Dir.mktmpdir
    setting = JekyllSetting.new(
      jekyll_path: dir,
      repository_type: "local",
      front_matter_mapping_json: "{\"layout\":\"post\"}"
    )

    assert setting.valid?
    assert_equal({ "layout" => "post" }, setting.front_matter_mapping)
  ensure
    FileUtils.remove_entry(dir) if dir
  end

  test "invalid front_matter_mapping_json adds error" do
    dir = Dir.mktmpdir
    setting = JekyllSetting.new(
      jekyll_path: dir,
      repository_type: "local",
      front_matter_mapping_json: "{not-json}"
    )

    refute setting.valid?
    assert setting.errors[:front_matter_mapping_json].any?
  ensure
    FileUtils.remove_entry(dir) if dir
  end
end
