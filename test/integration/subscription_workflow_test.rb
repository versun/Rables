# frozen_string_literal: true

require "test_helper"

class SubscriptionWorkflowTest < ActionDispatch::IntegrationTest
  def captcha_params(a: 3, b: 4, op: "+", answer: nil)
    expected = op == "+" ? (a + b) : (a - b)
    { captcha: { a:, b:, op:, answer: (answer || expected).to_s } }
  end

  test "complete subscription workflow" do
    # Step 1: Subscribe
    assert_difference "Subscriber.count", 1 do
      post subscriptions_path, params: {
        subscription: {
          email: "workflow@example.com"
        }
      }.merge(captcha_params)
    end

    subscriber = Subscriber.find_by(email: "workflow@example.com")
    assert_not_nil subscriber
    assert_not subscriber.confirmed?
    assert_not_nil subscriber.confirmation_token

    # Step 2: Confirm subscription
    get confirm_subscription_path(token: subscriber.confirmation_token)

    subscriber.reload
    assert subscriber.confirmed?
    assert subscriber.active?

    # Step 3: Unsubscribe
    get unsubscribe_path(token: subscriber.unsubscribe_token)

    subscriber.reload
    assert subscriber.unsubscribed?
    assert_not subscriber.active?
  end

  test "subscription with tags workflow" do
    tag1 = tags(:ruby)
    tag2 = tags(:rails)

    # Subscribe
    post subscriptions_path, params: {
      subscription: {
        email: "tagged@example.com"
      }
    }.merge(captcha_params)

    subscriber = Subscriber.find_by(email: "tagged@example.com")

    # Confirm
    get confirm_subscription_path(token: subscriber.confirmation_token)
    subscriber.reload

    # Add tags (assuming there's an admin interface for this)
    # This would typically be done through an admin interface
    subscriber.tags << [ tag1, tag2 ]

    assert_equal 2, subscriber.tags.count
    assert subscriber.subscribed_to_tag?(tag1)
    assert subscriber.subscribed_to_tag?(tag2)

    # Verify scope works
    subscribers_with_ruby = Subscriber.subscribed_to_tag(tag1)
    assert_includes subscribers_with_ruby, subscriber
  end

  test "duplicate subscription prevention" do
    existing_subscriber = subscribers(:confirmed_subscriber)

    # Try to subscribe with same email - should not create new record
    assert_no_difference "Subscriber.count" do
      post subscriptions_path, params: {
        subscription: {
          email: existing_subscriber.email
        }
      }.merge(captcha_params)
    end
  end

  test "subscription confirmation with invalid token" do
    get confirm_subscription_path(token: "invalid-token-12345")
    # The controller renders the confirm view regardless, just with @success = false
    assert_response :success
  end

  test "unsubscribe with invalid token" do
    get unsubscribe_path(token: "invalid-token-12345")
    # The controller renders the unsubscribe view regardless, just with @success = false
    assert_response :success
  end
end
