# frozen_string_literal: true

require "test_helper"

class Admin::TwitterSyncControllerTest < ActionDispatch::IntegrationTest
  setup do
    @user = users(:admin)
    sign_in(@user)
    @sync = TwitterSync.instance
  end

  test "show renders the sync settings" do
    get admin_twitter_sync_path

    assert_response :success
    assert_select "form[action=?]", admin_twitter_sync_path
    assert_select "input[name='twitter_sync[username]']"
    assert_select "input[name='twitter_sync[enabled]']"
    assert_select "select[name='twitter_sync[sync_schedule]']"
    assert_select "input[name='twitter_sync[start_date]']"
  end

  test "update saves enabled and username" do
    patch admin_twitter_sync_path, params: {
      twitter_sync: { enabled: "1", username: "@versun" }
    }

    assert_redirected_to admin_twitter_sync_path

    @sync.reload
    assert @sync.enabled?
    assert_equal "versun", @sync.username
  end

  test "update clears user_id and since_id when username changes" do
    @sync.update!(enabled: false, username: "olduser", user_id: "111", since_id: "222")

    patch admin_twitter_sync_path, params: {
      twitter_sync: { enabled: "1", username: "newuser" }
    }

    @sync.reload
    assert_equal "newuser", @sync.username
    assert_nil @sync.user_id
    assert_nil @sync.since_id
  end

  test "update keeps user_id and since_id when username is unchanged" do
    @sync.update!(enabled: false, username: "sameuser", user_id: "111", since_id: "222")

    patch admin_twitter_sync_path, params: {
      twitter_sync: { enabled: "0", username: "sameuser" }
    }

    @sync.reload
    assert_equal "111", @sync.user_id
    assert_equal "222", @sync.since_id
  end

  test "update with enabled but blank username re-renders with an error" do
    patch admin_twitter_sync_path, params: {
      twitter_sync: { enabled: "1", username: "" }
    }

    assert_redirected_to admin_twitter_sync_path
    follow_redirect!
    assert_not @sync.reload.enabled?
  end

  test "update saves sync_schedule" do
    patch admin_twitter_sync_path, params: {
      twitter_sync: { enabled: "0", username: "", sync_schedule: "daily" }
    }

    assert_redirected_to admin_twitter_sync_path
    assert_equal "daily", @sync.reload.sync_schedule
  end

  test "update saves start_date and resets the sync cursor when it changes" do
    @sync.update!(since_id: "999", user_id: "111")

    patch admin_twitter_sync_path, params: {
      twitter_sync: { enabled: "0", username: "", sync_schedule: "hourly", start_date: "2026-07-01" }
    }

    @sync.reload
    assert_equal Date.new(2026, 7, 1), @sync.start_date
    assert_nil @sync.since_id
    assert_equal "111", @sync.user_id
  end

  test "update keeps the sync cursor when start_date is unchanged" do
    @sync.update!(since_id: "999", start_date: Date.new(2026, 7, 1))

    patch admin_twitter_sync_path, params: {
      twitter_sync: { enabled: "0", username: "", sync_schedule: "hourly", start_date: "2026-07-01" }
    }

    assert_equal "999", @sync.reload.since_id
  end

  test "sync_now enqueues SyncTwitterJob with force" do
    assert_enqueued_with(job: SyncTwitterJob, args: [ { force: true } ]) do
      post sync_now_admin_twitter_sync_path
    end

    assert_redirected_to admin_twitter_sync_path
  end

  test "requires authentication" do
    delete session_path

    get admin_twitter_sync_path
    assert_redirected_to new_session_path

    patch admin_twitter_sync_path, params: { twitter_sync: { enabled: "1", username: "x" } }
    assert_redirected_to new_session_path

    post sync_now_admin_twitter_sync_path
    assert_redirected_to new_session_path
  end
end
