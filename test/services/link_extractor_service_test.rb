# frozen_string_literal: true

require "test_helper"

class LinkExtractorServiceTest < ActiveSupport::TestCase
  test "extracts source_url when present" do
    article = create_published_article(
      source_url: "https://example.org/original"
    )
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_includes links, "https://example.org/original"
  end

  test "extracts links from html content" do
    article = create_published_article(
      html_content: '<p>Check out <a href="https://rails.org">Rails</a> and <a href="https://ruby-lang.org">Ruby</a></p>'
    )
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_includes links, "https://rails.org"
    assert_includes links, "https://ruby-lang.org"
  end

  test "deduplicates links" do
    article = create_published_article(
      source_url: "https://example.org/page",
      html_content: '<p><a href="https://example.org/page">Link 1</a> and <a href="https://example.org/page">Link 2</a></p>'
    )
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_equal 1, links.count { |l| l == "https://example.org/page" }
  end

  test "excludes localhost URLs" do
    article = create_published_article(
      html_content: '<p><a href="http://localhost:3000/test">Local</a></p>'
    )
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_empty links
  end

  test "excludes 127.0.0.1 URLs" do
    article = create_published_article(
      html_content: '<p><a href="http://127.0.0.1:3000/test">Local</a></p>'
    )
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_empty links
  end

  test "excludes example.com URLs" do
    article = create_published_article(
      html_content: '<p><a href="https://example.com/test">Example</a></p>'
    )
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_empty links
  end

  test "rejects invalid URLs" do
    article = create_published_article(
      html_content: '<p><a href="not-a-valid-url">Invalid</a></p>'
    )
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_empty links
  end

  test "rejects non-http URLs" do
    article = create_published_article(
      html_content: '<p><a href="ftp://files.example.org/file">FTP</a> and <a href="mailto:test@example.org">Email</a></p>'
    )
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_empty links
  end

  test "returns empty array for blank content" do
    article = create_published_article(html_content: "<p>placeholder</p>")
    article.update_column(:html_content, "")
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_equal [], links
  end

  test "handles article with rich text content" do
    article = Article.new(
      title: "Rich Text Test",
      slug: "rich-text-test-#{Time.current.to_i}",
      status: :publish,
      content_type: :rich_text
    )
    article.content = ActionText::Content.new('<p><a href="https://test.org">Test</a></p>')
    article.save!
    service = LinkExtractorService.new(article)
    links = service.extract_links

    assert_includes links, "https://test.org"
  end
end
