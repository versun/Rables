# frozen_string_literal: true

require "test_helper"

class SubscriberTagTest < ActiveSupport::TestCase
  test "enforces unique subscriber/tag pair" do
    subscriber = subscribers(:confirmed_subscriber)
    tag = tags(:ruby)

    SubscriberTag.create!(subscriber: subscriber, tag: tag)
    duplicate = SubscriberTag.new(subscriber: subscriber, tag: tag)

    assert_not duplicate.valid?
    assert_includes duplicate.errors[:subscriber_id], "has already been taken"
  end
end
