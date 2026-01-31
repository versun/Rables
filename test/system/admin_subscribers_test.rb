# frozen_string_literal: true

require "application_system_test_case"

class AdminSubscribersTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @confirmed = subscribers(:confirmed_subscriber)
    @unconfirmed = subscribers(:unconfirmed_subscriber)
    @unsubscribed = subscribers(:unsubscribed_subscriber)
  end

  test "viewing subscribers index" do
    sign_in(@user)
    visit admin_subscribers_path

    assert_text "订阅者管理"
    assert_text @confirmed.email
    assert_text @unconfirmed.email
  end

  test "batch creating subscribers" do
    sign_in(@user)
    visit admin_subscribers_path

    emails_text = "batch1@example.com,news\nbatch2@example.com"
    fill_in "emails_text", with: emails_text
    click_button "批量添加"

    assert_text "成功添加"
    assert Subscriber.exists?(email: "batch1@example.com")
    assert Subscriber.exists?(email: "batch2@example.com")
    assert Tag.exists?(name: "news")
  end

  test "filtering by active subscribers" do
    sign_in(@user)
    visit admin_subscribers_path

    select "已确认", from: "status"
    click_button "筛选"

    assert_text @confirmed.email
    assert_no_text @unconfirmed.email
  end

  test "filtering by unconfirmed subscribers" do
    sign_in(@user)
    visit admin_subscribers_path

    select "待确认", from: "status"
    click_button "筛选"

    assert_text @unconfirmed.email
    assert_no_text @confirmed.email
  end

  test "deleting a subscriber" do
    skip "This test requires JavaScript support (Selenium)" unless self.class.use_selenium?

    sign_in(@user)
    visit admin_subscribers_path

    accept_confirm do
      within("tr", text: @unsubscribed.email) do
        click_link "删除"
      end
    end

    assert_text "订阅者已删除。"
    assert_not Subscriber.exists?(id: @unsubscribed.id)
  end
end
