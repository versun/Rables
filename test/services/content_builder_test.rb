# frozen_string_literal: true

require "test_helper"

class ContentBuilderTest < ActiveSupport::TestCase
  class DummyBuilder
    include ContentBuilder
  end

  setup do
    @builder = DummyBuilder.new
    @source_url = "https://example.com/source"
  end

  test "without truncation: no read more and reference url is last (with title)" do
    article = create_published_article(
      title: "Hello",
      html_content: "<p>Short content</p>",
      description: nil,
      source_url: @source_url
    )

    post = @builder.build_content(article: article, max_length: 300)

    assert_not_includes post, "Read more:"
    assert_includes post, @source_url
    assert post.end_with?(@source_url)
  end

  test "with truncation: has read more and reference url is last (with title)" do
    article = create_published_article(
      title: "Hello",
      html_content: "<p>#{'a' * 200}</p>",
      description: nil,
      source_url: @source_url
    )

    post = @builder.build_content(article: article, max_length: 60)

    assert_includes post, "Read more:"
    assert_includes post, @source_url
    assert_operator post.index("Read more:"), :<, post.index(@source_url)
    assert post.end_with?(@source_url)
  end

  test "without truncation: no read more and reference url is last (no title)" do
    article = create_published_article(
      title: nil,
      html_content: "<p>Short content</p>",
      description: nil,
      source_url: @source_url
    )

    post = @builder.build_content(article: article, max_length: 300)

    assert_not_includes post, "Read more:"
    assert_includes post, @source_url
    assert post.end_with?(@source_url)
  end

  test "with truncation: has read more and reference url is last (no title)" do
    article = create_published_article(
      title: nil,
      html_content: "<p>#{'a' * 200}</p>",
      description: nil,
      source_url: @source_url
    )

    post = @builder.build_content(article: article, max_length: 60)

    assert_includes post, "Read more:"
    assert_includes post, @source_url
    assert_operator post.index("Read more:"), :<, post.index(@source_url)
    assert post.end_with?(@source_url)
  end

  test "with description: always adds read more link even for short content" do
    article = create_published_article(
      title: "Hello",
      html_content: "<p>Long content that will be ignored</p>",
      description: "Short desc",
      source_url: nil
    )

    post = @builder.build_content(article: article, max_length: 300)

    assert_includes post, "Read more:"
    assert_includes post, "Short desc"
    assert_not_includes post, "Long content"
  end

  test "with description and source_url: read more before source_url" do
    article = create_published_article(
      title: "Hello",
      html_content: "<p>Long content</p>",
      description: "Short desc",
      source_url: @source_url
    )

    post = @builder.build_content(article: article, max_length: 300)

    assert_includes post, "Read more:"
    assert_includes post, @source_url
    assert_operator post.index("Read more:"), :<, post.index(@source_url)
    assert post.end_with?(@source_url)
  end

  # 场景1: description和content同时存在时，发布内容为标题+description+read more链接
  test "scenario 1: with description and content, outputs title + description + read more link" do
    max_length = 100
    article = create_published_article(
      title: "Test Title",
      html_content: "<p>This is the full article content that should be ignored</p>",
      description: "This is desc",
      source_url: nil
    )

    post = @builder.build_content(article: article, max_length: max_length)

    # 验证内容结构
    assert_includes post, "Test Title"
    assert_includes post, "This is desc"
    assert_includes post, "Read more:"
    assert_not_includes post, "full article content"

    # 验证字符数不超过最大值
    assert_operator post.length, :<=, max_length, "Post length #{post.length} exceeds max_length #{max_length}"
  end

  # 场景2: description为空，content不为空，总字符不超过最大值，不添加read more链接
  test "scenario 2: no description, short content within max_length, no read more link" do
    max_length = 100
    article = create_published_article(
      title: "Hi",
      html_content: "<p>Short</p>",
      description: nil,
      source_url: nil
    )

    post = @builder.build_content(article: article, max_length: max_length)

    # 验证内容结构
    assert_includes post, "Hi"
    assert_includes post, "Short"
    assert_not_includes post, "Read more:"

    # 验证字符数不超过最大值
    assert_operator post.length, :<=, max_length, "Post length #{post.length} exceeds max_length #{max_length}"
  end

  # 场景3: description为空，content不为空，总字符超过最大值，添加read more链接并截断
  test "scenario 3: no description, long content exceeds max_length, adds read more link and truncates" do
    max_length = 80
    long_content = "a" * 200
    article = create_published_article(
      title: "Title",
      html_content: "<p>#{long_content}</p>",
      description: nil,
      source_url: nil
    )

    post = @builder.build_content(article: article, max_length: max_length)

    # 验证内容结构
    assert_includes post, "Title"
    assert_includes post, "Read more:"
    assert_includes post, "...", "Content should be truncated with ellipsis"

    # 验证字符数不超过最大值
    assert_operator post.length, :<=, max_length, "Post length #{post.length} exceeds max_length #{max_length}"
  end
end
