# frozen_string_literal: true

require "test_helper"

class Admin::JekyllControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "shows jekyll settings page" do
    get admin_jekyll_path
    assert_response :success
  end

  test "updates jekyll settings" do
    dir = Dir.mktmpdir

    patch admin_jekyll_path, params: {
      jekyll_setting: {
        jekyll_path: dir,
        repository_type: "local",
        posts_directory: "_posts",
        pages_directory: "_pages"
      }
    }

    assert_redirected_to admin_jekyll_path
    assert_equal dir, JekyllSetting.instance.reload.jekyll_path
  ensure
    FileUtils.remove_entry(dir) if dir
  end

  test "sync enqueues job" do
    assert_enqueued_with(job: JekyllSyncJob) do
      post sync_admin_jekyll_path
    end
  end
end
