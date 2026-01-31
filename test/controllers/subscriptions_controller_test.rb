# frozen_string_literal: true

require "test_helper"

class SubscriptionsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @subscriber = subscribers(:confirmed_subscriber)
  end

  def captcha_params(a: 3, b: 4, op: "+", answer: nil)
    expected = op == "+" ? (a + b) : (a - b)
    { captcha: { a:, b:, op:, answer: (answer || expected).to_s } }
  end

  test "should create subscription" do
    assert_difference "Subscriber.count", 1 do
      post subscriptions_path, params: {
        subscription: {
          email: "new@example.com"
        }
      }.merge(captcha_params), as: :json
    end

    assert_response :success
  end

  test "should not create subscription with invalid email" do
    assert_no_difference "Subscriber.count" do
      post subscriptions_path, params: {
        subscription: {
          email: "invalid-email"
        }
      }.merge(captcha_params), as: :json
    end
    assert_response :unprocessable_entity
  end

  test "should not create duplicate subscription" do
    assert_no_difference "Subscriber.count" do
      post subscriptions_path, params: {
        subscription: {
          email: @subscriber.email
        }
      }.merge(captcha_params), as: :json
    end
    assert_response :success
  end

  test "should confirm subscription with valid token" do
    subscriber = subscribers(:unconfirmed_subscriber)

    get confirm_subscription_path(token: subscriber.confirmation_token)

    subscriber.reload
    assert subscriber.confirmed?
  end

  test "should not confirm subscription with invalid token" do
    get confirm_subscription_path(token: "invalid-token")
    assert_response :success
    assert_includes response.body, "无效的确认链接"
  end

  test "should unsubscribe with valid token" do
    subscriber = subscribers(:confirmed_subscriber)

    get unsubscribe_path(token: subscriber.unsubscribe_token)

    subscriber.reload
    assert subscriber.unsubscribed?
  end

  test "should not unsubscribe with invalid token" do
    get unsubscribe_path(token: "invalid-token")
    assert_response :success
    assert_includes response.body, "无效的取消订阅链接"
  end

  test "blank email redirects with alert" do
    post subscriptions_path, params: { subscription: { email: "" } }
    assert_redirected_to root_path
    assert_match "请输入有效的邮箱地址", flash[:alert]
  end

  test "captcha failure returns json error" do
    post subscriptions_path, params: {
      subscription: { email: "captcha@example.com" },
      captcha: { a: "1", b: "2", op: "+", answer: "" }
    }, as: :json

    assert_response :unprocessable_entity
    assert_equal false, response.parsed_body["success"]
  end

  test "creates subscription with tags" do
    tag = tags(:ruby)

    assert_difference "Subscriber.count", 1 do
      post subscriptions_path, params: {
        subscription: {
          email: "tagged@example.com",
          tag_ids: [ tag.id ]
        }
      }.merge(captcha_params), as: :json
    end

    subscriber = Subscriber.find_by!(email: "tagged@example.com")
    assert_includes subscriber.tags, tag
  end
end
