# frozen_string_literal: true

require "test_helper"

class UrlValidatorTest < ActiveSupport::TestCase
  class TestModel
    include ActiveModel::Model
    attr_accessor :url

    validates :url, url: true
  end

  test "accepts valid http and https URLs" do
    assert TestModel.new(url: "https://example.com/path?q=1#frag").valid?
    assert TestModel.new(url: "http://example.com:8080").valid?
  end

  test "rejects URL with empty host" do
    record = TestModel.new(url: "http://")

    assert_not record.valid?
    assert_includes record.errors[:url], "is not a valid URL"
  end

  test "rejects URL with only a scheme and slashes" do
    assert_not TestModel.new(url: "https://").valid?
  end

  test "rejects non-http schemes" do
    assert_not TestModel.new(url: "ftp://example.com/file").valid?
  end

  test "rejects malformed URLs" do
    assert_not TestModel.new(url: "not a url").valid?
  end
end
