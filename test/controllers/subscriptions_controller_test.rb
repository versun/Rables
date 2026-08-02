# frozen_string_literal: true

require "test_helper"

class SubscriptionsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @subscriber = subscribers(:confirmed_subscriber)
  end

  def captcha_params(a: 3, b: 4, op: "+", answer: nil)
    expected = op == "+" ? (a + b) : (a - b)
    token = MathCaptchaHelper.sign_math_captcha({ a: a, b: b, op: op })
    { captcha: { a:, b:, op:, token:, answer: (answer || expected).to_s } }
  end

  # The POST /unsubscribe route is added separately; draw it locally so the
  # two-step flow can be exercised end to end.
  def with_unsubscribe_post_route(&block)
    with_routing do |set|
      set.draw do
        root "articles#index"
        get "/unsubscribe", to: "subscriptions#unsubscribe", as: :unsubscribe
        post "/unsubscribe", to: "subscriptions#unsubscribe"
      end
      block.call
    end
  end

  # Simulate the rate limit counter already exceeding the threshold
  def with_rate_limit_count(count)
    cache = Rails.cache
    cache.define_singleton_method(:increment) { |*| count }
    yield
  ensure
    cache.singleton_class.send(:remove_method, :increment)
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

  test "should allow resubscribe for unsubscribed subscriber" do
    subscriber = subscribers(:unsubscribed_subscriber)
    old_token = subscriber.confirmation_token

    assert_no_difference "Subscriber.count" do
      assert_enqueued_with(job: NewsletterConfirmationJob, args: [ subscriber.id ]) do
        post subscriptions_path, params: {
          subscription: {
            email: subscriber.email
          }
        }.merge(captcha_params), as: :json
      end
    end

    assert_response :success

    subscriber.reload
    assert_nil subscriber.confirmed_at
    assert_nil subscriber.unsubscribed_at
    assert_not_equal old_token, subscriber.confirmation_token
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

  test "unsubscribe link only renders confirmation page" do
    subscriber = subscribers(:confirmed_subscriber)

    get unsubscribe_path(token: subscriber.unsubscribe_token)

    assert_response :success
    assert_includes response.body, "确认取消订阅"
    assert_includes response.body, subscriber.unsubscribe_token

    subscriber.reload
    assert_not subscriber.unsubscribed?
  end

  test "unsubscribe confirmation page handles invalid token" do
    get unsubscribe_path(token: "invalid-token")
    assert_response :success
    assert_includes response.body, "无效的取消订阅链接"
  end

  test "posting unsubscribe performs the unsubscribe" do
    subscriber = subscribers(:confirmed_subscriber)

    with_unsubscribe_post_route do
      post "/unsubscribe", params: { token: subscriber.unsubscribe_token }

      assert_response :success
      assert_includes response.body, "取消订阅成功"
    end

    subscriber.reload
    assert subscriber.unsubscribed?
  end

  test "posting unsubscribe with invalid token does nothing" do
    with_unsubscribe_post_route do
      post "/unsubscribe", params: { token: "invalid-token" }

      assert_response :success
      assert_includes response.body, "取消订阅失败"
    end
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

  test "captcha with tampered token returns json error" do
    tampered = captcha_params
    tampered[:captcha][:token] = "#{tampered[:captcha][:token]}tampered"

    assert_no_difference "Subscriber.count" do
      post subscriptions_path, params: {
        subscription: { email: "captcha@example.com" }
      }.merge(tampered), as: :json
    end

    assert_response :unprocessable_entity
    assert_equal false, response.parsed_body["success"]
  end

  test "captcha with expired token returns json error" do
    expired = captcha_params

    travel(MathCaptchaHelper::MATH_CAPTCHA_TTL + 1.minute) do
      assert_no_difference "Subscriber.count" do
        post subscriptions_path, params: {
          subscription: { email: "captcha@example.com" }
        }.merge(expired), as: :json
      end
    end

    assert_response :unprocessable_entity
    assert_equal false, response.parsed_body["success"]
  end

  test "rate limits subscription creation per ip" do
    assert_no_difference "Subscriber.count" do
      with_rate_limit_count(6) do
        post subscriptions_path, params: {
          subscription: { email: "limited@example.com" }
        }.merge(captcha_params)
      end
    end

    assert_redirected_to root_path
    assert_match "请稍后再试", flash[:alert]
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
