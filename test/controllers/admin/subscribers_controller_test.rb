# frozen_string_literal: true

require "test_helper"

class Admin::SubscribersControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)

    @confirmed = subscribers(:confirmed_subscriber)
    @unconfirmed = subscribers(:unconfirmed_subscriber)
    @unsubscribed = subscribers(:unsubscribed_subscriber)

    @ruby = tags(:ruby)
    @rails = tags(:rails)

    @confirmed.tags << @ruby
    @unsubscribed.tags << @rails
  end

  test "index filters by status" do
    get admin_subscribers_path(status: "unconfirmed")

    assert_response :success
    assert_select "td", text: @unconfirmed.email
    assert_select "td", text: @confirmed.email, count: 0
    assert_select "td", text: @unsubscribed.email, count: 0
  end

  test "index filters by subscription content" do
    get admin_subscribers_path(include_all: "1")

    assert_response :success
    assert_select "td", text: @unconfirmed.email
    assert_select "td", text: @confirmed.email, count: 0
    assert_select "td", text: @unsubscribed.email, count: 0

    get admin_subscribers_path(tag_ids: [ @ruby.id ])

    assert_response :success
    assert_select "td", text: @confirmed.email
    assert_select "td", text: @unconfirmed.email, count: 0
    assert_select "td", text: @unsubscribed.email, count: 0

    get admin_subscribers_path(include_all: "1", tag_ids: [ @rails.id ])

    assert_response :success
    assert_select "td", text: @unconfirmed.email
    assert_select "td", text: @unsubscribed.email
    assert_select "td", text: @confirmed.email, count: 0
  end

  test "batch_confirm confirms and reactivates subscribers" do
    assert_not @unconfirmed.confirmed?
    assert_not @unsubscribed.active?

    post batch_confirm_admin_subscribers_path, params: { ids: [ @unconfirmed.id, @unsubscribed.id ] }

    assert_redirected_to admin_subscribers_path

    @unconfirmed.reload
    @unsubscribed.reload

    assert @unconfirmed.confirmed?
    assert_nil @unconfirmed.unsubscribed_at

    assert @unsubscribed.active?
  end

  test "batch_destroy deletes selected subscribers" do
    assert_difference "Subscriber.count", -1 do
      post batch_destroy_admin_subscribers_path, params: { ids: [ @confirmed.id ] }
    end

    assert_redirected_to admin_subscribers_path
  end

  test "batch_create handles errors and destroy removes subscriber" do
    get admin_subscribers_path(status: "active")
    assert_response :success

    get admin_subscribers_path(status: "unsubscribed")
    assert_response :success

    post batch_create_admin_subscribers_path, params: { emails_text: "" }
    assert_redirected_to admin_subscribers_path
    assert_match "请输入邮箱地址", flash[:alert]

    emails_text = <<~TEXT
      valid@example.com,newtag
      invalid-email
    TEXT

    post batch_create_admin_subscribers_path, params: { emails_text: emails_text }
    assert_redirected_to admin_subscribers_path
    assert Subscriber.find_by(email: "valid@example.com")
    assert_match "成功添加", flash[:notice]

    subscriber = Subscriber.create!(email: "remove@example.com")
    delete admin_subscriber_path(subscriber)
    assert_redirected_to admin_subscribers_path
    assert_nil Subscriber.find_by(id: subscriber.id)
  end
end
