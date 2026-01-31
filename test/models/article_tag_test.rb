# frozen_string_literal: true

require "test_helper"

class ArticleTagTest < ActiveSupport::TestCase
  test "enforces unique article/tag pair" do
    article = articles(:published_article)
    tag = tags(:ruby)

    ArticleTag.create!(article: article, tag: tag)
    duplicate = ArticleTag.new(article: article, tag: tag)

    assert_not duplicate.valid?
    assert_includes duplicate.errors[:article_id], "has already been taken"
  end
end
