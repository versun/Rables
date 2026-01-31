# frozen_string_literal: true

require "test_helper"

class SubscriberTest < ActiveSupport::TestCase
  def setup
    @subscriber = Subscriber.new(
      email: "test@example.com"
    )
  end

  test "should be valid with valid email" do
    assert @subscriber.valid?
  end

  test "should require email" do
    @subscriber.email = nil
    assert_not @subscriber.valid?
    assert_includes @subscriber.errors[:email], "can't be blank"
  end

  test "should require unique email" do
    existing_subscriber = subscribers(:confirmed_subscriber)
    @subscriber.email = existing_subscriber.email
    assert_not @subscriber.valid?
    assert_includes @subscriber.errors[:email], "has already been taken"
  end

  test "should validate email format" do
    @subscriber.email = "invalid-email"
    assert_not @subscriber.valid?
    assert_includes @subscriber.errors[:email], "is invalid"
  end

  test "should accept valid email formats" do
    valid_emails = [
      "test@example.com",
      "user.name@example.co.uk",
      "user+tag@example.com"
    ]

    valid_emails.each do |email|
      @subscriber.email = email
      assert @subscriber.valid?, "#{email} should be valid"
    end
  end

  test "should generate tokens before create" do
    @subscriber.save!
    assert_not_nil @subscriber.confirmation_token
    assert_not_nil @subscriber.unsubscribe_token
  end

  test "confirmed scope should return only confirmed subscribers" do
    confirmed = subscribers(:confirmed_subscriber)
    unconfirmed = subscribers(:unconfirmed_subscriber)

    confirmed_subscribers = Subscriber.confirmed
    assert_includes confirmed_subscribers, confirmed
    assert_not_includes confirmed_subscribers, unconfirmed
  end

  test "active scope should return confirmed and not unsubscribed" do
    active = subscribers(:confirmed_subscriber)
    unsubscribed = subscribers(:unsubscribed_subscriber)

    active_subscribers = Subscriber.active
    assert_includes active_subscribers, active
    assert_not_includes active_subscribers, unsubscribed
  end

  test "confirmed? should return true when confirmed_at is present" do
    subscriber = subscribers(:confirmed_subscriber)
    assert subscriber.confirmed?
  end

  test "confirmed? should return false when confirmed_at is nil" do
    subscriber = subscribers(:unconfirmed_subscriber)
    assert_not subscriber.confirmed?
  end

  test "active? should return true for confirmed and not unsubscribed" do
    subscriber = subscribers(:confirmed_subscriber)
    assert subscriber.active?
  end

  test "active? should return false for unsubscribed" do
    subscriber = subscribers(:unsubscribed_subscriber)
    assert_not subscriber.active?
  end

  test "confirm! should set confirmed_at" do
    subscriber = subscribers(:unconfirmed_subscriber)
    assert_nil subscriber.confirmed_at

    subscriber.confirm!
    assert_not_nil subscriber.confirmed_at
  end

  test "confirm! should not update if already confirmed" do
    subscriber = subscribers(:confirmed_subscriber)
    original_confirmed_at = subscriber.confirmed_at

    subscriber.confirm!
    assert_equal original_confirmed_at, subscriber.confirmed_at
  end

  test "unsubscribe! should set unsubscribed_at" do
    subscriber = subscribers(:confirmed_subscriber)
    assert_nil subscriber.unsubscribed_at

    subscriber.unsubscribe!
    assert_not_nil subscriber.unsubscribed_at
  end

  test "unsubscribed? should return true when unsubscribed_at is present" do
    subscriber = subscribers(:unsubscribed_subscriber)
    assert subscriber.unsubscribed?
  end

  test "subscribed_to_tag? should return true when subscriber has tag" do
    subscriber = subscribers(:confirmed_subscriber)
    tag = tags(:ruby)
    subscriber.tags << tag

    assert subscriber.subscribed_to_tag?(tag)
  end

  test "subscribed_to_tag? should return false when subscriber does not have tag" do
    subscriber = subscribers(:confirmed_subscriber)
    tag = tags(:ruby)

    assert_not subscriber.subscribed_to_tag?(tag)
  end

  test "subscribed_to_all? should return true when no tags" do
    subscriber = subscribers(:confirmed_subscriber)
    assert subscriber.subscribed_to_all?
  end

  test "subscribed_to_all? should return false when has tags" do
    subscriber = subscribers(:confirmed_subscriber)
    tag = tags(:ruby)
    subscriber.tags << tag

    assert_not subscriber.subscribed_to_all?
  end

  test "should have many tags through subscriber_tags" do
    subscriber = subscribers(:confirmed_subscriber)
    assert_respond_to subscriber, :tags
  end

  test "should destroy associated subscriber_tags when destroyed" do
    subscriber = subscribers(:confirmed_subscriber)
    tag = tags(:ruby)
    subscriber.tags << tag

    assert_difference "SubscriberTag.count", -1 do
      subscriber.destroy
    end
  end

  test "subscribed_to_tag scope should find subscribers with tag" do
    subscriber = subscribers(:confirmed_subscriber)
    tag = tags(:ruby)
    subscriber.tags << tag

    results = Subscriber.subscribed_to_tag(tag)
    assert_includes results, subscriber
  end

  test "subscribed_to_any_tags scope should find subscribers with any of the tags" do
    subscriber1 = subscribers(:confirmed_subscriber)
    subscriber2 = create_subscriber(email: "another@example.com")

    tag1 = tags(:ruby)
    tag2 = tags(:rails)

    subscriber1.tags << tag1
    subscriber2.tags << tag2

    results = Subscriber.subscribed_to_any_tags([ tag1.id, tag2.id ])
    assert_includes results, subscriber1
    assert_includes results, subscriber2
  end

  test "subscribed_to_all scope should find subscribers without tags" do
    subscriber_with_tags = subscribers(:confirmed_subscriber)
    subscriber_with_tags.tags << tags(:ruby)

    subscriber_without_tags = create_subscriber(email: "no-tags@example.com")

    results = Subscriber.subscribed_to_all
    assert_not_includes results, subscriber_with_tags
    assert_includes results, subscriber_without_tags
  end
end
