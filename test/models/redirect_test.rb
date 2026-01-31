# frozen_string_literal: true

require "test_helper"

class RedirectTest < ActiveSupport::TestCase
  def setup
    @redirect = Redirect.new(
      regex: "^/old-path$",
      replacement: "/new-path",
      enabled: true,
      permanent: false
    )
  end

  test "should be valid with valid attributes" do
    assert @redirect.valid?
  end

  test "should require regex" do
    @redirect.regex = nil
    assert_not @redirect.valid?
  end

  test "should require replacement" do
    @redirect.replacement = nil
    assert_not @redirect.valid?
  end

  test "should default enabled to true" do
    redirect = Redirect.new(
      regex: "^/test$",
      replacement: "/new-test"
    )
    assert redirect.enabled?
  end

  test "should default permanent to false" do
    redirect = Redirect.new(
      regex: "^/test$",
      replacement: "/new-test"
    )
    assert_not redirect.permanent?
  end

  test "enabled scope should return only enabled redirects" do
    enabled_redirect = Redirect.create!(
      regex: "^/enabled$",
      replacement: "/new-enabled",
      enabled: true
    )

    disabled_redirect = Redirect.create!(
      regex: "^/disabled$",
      replacement: "/new-disabled",
      enabled: false
    )

    enabled = Redirect.enabled
    assert_includes enabled, enabled_redirect
    assert_not_includes enabled, disabled_redirect
  end

  test "match? should return true for matching path" do
    redirect = Redirect.create!(
      regex: "^/old-article$",
      replacement: "/new-article"
    )

    assert redirect.match?("/old-article")
    assert_not redirect.match?("/old-article/extra")
  end

  test "match? should return false when disabled" do
    redirect = Redirect.create!(
      regex: "^/old-article$",
      replacement: "/new-article",
      enabled: false
    )

    assert_not redirect.match?("/old-article")
  end

  test "apply_to should return replacement for matching path" do
    redirect = Redirect.create!(
      regex: "^/old-article$",
      replacement: "/new-article"
    )

    assert_equal "/new-article", redirect.apply_to("/old-article")
    assert_nil redirect.apply_to("/other-path")
  end

  test "should validate regex pattern" do
    @redirect.regex = "[invalid regex"
    assert_not @redirect.valid?
    assert_includes @redirect.errors[:regex].first, "is not a valid regular expression"
  end
end
