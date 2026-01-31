# frozen_string_literal: true

require "test_helper"

class SocialMediaPostTest < ActiveSupport::TestCase
  test "validates platform presence and uniqueness per article" do
    article = articles(:published_article)

    post = SocialMediaPost.new(article: article, platform: "twitter", url: "https://example.com/post-1")
    assert post.valid?

    SocialMediaPost.create!(article: article, platform: "twitter", url: "https://example.com/post-2")
    duplicate = SocialMediaPost.new(article: article, platform: "twitter", url: "https://example.com/post-3")
    assert_not duplicate.valid?
    assert_includes duplicate.errors[:platform], "has already been taken"

    missing_platform = SocialMediaPost.new(article: article, platform: nil, url: "https://example.com/post-4")
    assert_not missing_platform.valid?
    assert_includes missing_platform.errors[:platform], "can't be blank"
  end
end
