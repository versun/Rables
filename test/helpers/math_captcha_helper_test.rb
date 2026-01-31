# frozen_string_literal: true

require "test_helper"

class MathCaptchaHelperTest < ActionView::TestCase
  private

  def build_chooser(value)
    chooser = Object.new
    chooser.define_singleton_method(:sample) { value }
    chooser
  end

  def build_rng(*values)
    ranges = []
    rng = Object.new
    rng.define_singleton_method(:ranges) { ranges }
    rng.define_singleton_method(:rand) do |range|
      ranges << range
      values.shift
    end
    rng
  end

  public

  test "math_captcha_challenge formats question without question mark" do
    captcha = math_captcha_challenge(max: 10)

    assert_includes captcha.keys, :a
    assert_includes captcha.keys, :b
    assert_includes captcha.keys, :op
    assert_includes captcha.keys, :question

    assert_match(/\A\d+ [+-] \d+ =\z/, captcha[:question])
    refute_includes captcha[:question], "?"
  end

  test "math_captcha_challenge uses addition branch when sample returns true" do
    rng = build_rng(3, 3)
    chooser = build_chooser(true)
    captcha = math_captcha_challenge(max: 10, rng: rng, chooser: chooser)

    assert_equal "+", captcha[:op]
    assert_equal "3 + 3 =", captcha[:question]
    assert_equal [ 0..10, 0..7 ], rng.ranges
  end

  test "math_captcha_challenge uses subtraction branch and normalizes max" do
    rng = build_rng(0, 0)
    chooser = build_chooser(false)
    captcha = math_captcha_challenge(max: 0, rng: rng, chooser: chooser)

    assert_equal "-", captcha[:op]
    assert_equal "0 - 0 =", captcha[:question]
    assert_equal [ 0..10, 0..0 ], rng.ranges
  end
end
