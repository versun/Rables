# frozen_string_literal: true

require "test_helper"

class Admin::JekyllControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
    @setting = JekyllSetting.instance
    @setting.update(jekyll_path: "/tmp/test_jekyll", auto_sync_enabled: false)
  end

  test "should get show" do
    get admin_jekyll_path
    assert_response :success
  end

  test "should update setting" do
    patch admin_jekyll_path, params: {
      jekyll_setting: {
        jekyll_path: "/new/path",
        repository_type: "git",
        repository_url: "https://github.com/user/repo.git"
      }
    }

    assert_redirected_to admin_jekyll_path
    assert_equal "/new/path", @setting.reload.jekyll_path
  end

  test "should not update setting with invalid data" do
    patch admin_jekyll_path, params: {
      jekyll_setting: {
        front_matter_mapping: "invalid json"
      }
    }

    assert_response :unprocessable_entity
  end

  test "should trigger sync" do
    assert_enqueued_with(job: JekyllSyncJob) do
      post sync_admin_jekyll_path
    end

    assert_redirected_to admin_jekyll_path
  end

  test "should verify configuration" do
    # Create the directory so verification passes
    FileUtils.mkdir_p(@setting.jekyll_path)
    post verify_admin_jekyll_path
    assert_redirected_to admin_jekyll_path
  ensure
    FileUtils.rm_rf(@setting.jekyll_path)
  end

  test "should preview article" do
    article = articles(:published_article)
    get preview_admin_jekyll_path(article_slug: article.slug)

    assert_response :success
    assert_includes response.body, article.title
  end

  test "should redirect preview without article or page" do
    get preview_admin_jekyll_path

    assert_redirected_to admin_jekyll_path
  end
end
