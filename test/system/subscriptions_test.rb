# frozen_string_literal: true

require "application_system_test_case"

class SubscriptionsTest < ApplicationSystemTestCase
  def solve_math_captcha
    find("input[name='subscription[email]']", visible: :all).click
    assert_selector "input[name='captcha[answer]']", visible: true

    question_text = find("[data-math-captcha-target='question']", visible: :all).text
    match = question_text.match(/(\d+)\s*([+-])\s*(\d+)/)
    raise "Could not parse captcha question: #{question_text.inspect}" unless match

    a = match[1].to_i
    op = match[2]
    b = match[3].to_i
    expected = op == "+" ? (a + b) : (a - b)

    find("input[name='captcha[answer]']", visible: true).set(expected.to_s)
  end

  test "subscribing from the subscriptions page" do
    email = "subscriber-#{SecureRandom.hex(6)}@example.com"

    visit subscriptions_path
    fill_in "输入您的邮箱地址", with: email
    solve_math_captcha
    click_button "订阅"

    assert_current_path root_path
    assert_text "订阅成功！请检查您的邮箱并点击确认链接。"
  end

  test "confirming a subscription token" do
    subscriber = create_subscriber(email: "confirm-#{SecureRandom.hex(6)}@example.com", confirmed: false)

    visit confirm_subscription_path(token: subscriber.confirmation_token)

    assert_text "订阅确认成功"
    subscriber.reload
    assert subscriber.confirmed?
  end

  test "unsubscribing with a token" do
    subscriber = create_subscriber(email: "unsub-#{SecureRandom.hex(6)}@example.com", confirmed: true)

    visit unsubscribe_path(token: subscriber.unsubscribe_token)

    assert_text "取消订阅成功"
    subscriber.reload
    assert subscriber.unsubscribed?
  end
end
