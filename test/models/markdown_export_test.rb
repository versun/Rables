# frozen_string_literal: true

require "test_helper"

class MarkdownExportTest < ActiveSupport::TestCase
  test "generates a zip containing markdown files for articles and pages" do
    exporter = MarkdownExport.new

    assert exporter.generate, exporter.error_message
    assert exporter.zip_path.present?
    assert File.exist?(exporter.zip_path), "expected zip to exist at #{exporter.zip_path}"

    entries = []
    Zip::File.open(exporter.zip_path) do |zip|
      entries = zip.entries.map(&:name)

      article_entry = zip.find_entry("articles/published-article.md")
      assert article_entry, "expected articles/published-article.md to exist in zip"
      article_content = article_entry.get_input_stream.read
      assert_includes article_content, "type: article"
      assert_includes article_content, "title: Published Article"
      assert_includes article_content, "slug: published-article"
      assert_includes article_content, "Published article content"

      page_entry = zip.find_entry("pages/published-page-fixture.md")
      assert page_entry, "expected pages/published-page-fixture.md to exist in zip"
      page_content = page_entry.get_input_stream.read
      assert_includes page_content, "type: page"
      assert_includes page_content, "title: Published Page Fixture"
      assert_includes page_content, "slug: published-page-fixture"
      assert_includes page_content, "Published page content"
    end

    assert entries.any? { |e| e.start_with?("articles/") }, "expected at least one article markdown file"
    assert entries.any? { |e| e.start_with?("pages/") }, "expected at least one page markdown file"
  ensure
    File.delete(exporter.zip_path) if exporter&.zip_path.present? && File.exist?(exporter.zip_path)
  end

  test "includes article source reference above content" do
    exporter = MarkdownExport.new

    assert exporter.generate, exporter.error_message
    assert exporter.zip_path.present?
    assert File.exist?(exporter.zip_path), "expected zip to exist at #{exporter.zip_path}"

    Zip::File.open(exporter.zip_path) do |zip|
      article_entry = zip.find_entry("articles/source-article.md")
      assert article_entry, "expected articles/source-article.md to exist in zip"

      article_content = article_entry.get_input_stream.read
      assert_includes article_content, "Reference:"
      assert_includes article_content, "Source: Example Author"
      assert_includes article_content, "Example source quote."
      assert_includes article_content, "Original: https://example.com/source"

      reference_index = article_content.index("Reference:")
      content_index = article_content.index("Source article content")
      assert reference_index, "expected reference section to be present"
      assert content_index, "expected article content to be present"
      assert reference_index < content_index, "expected reference section to appear before article content"
    end
  ensure
    File.delete(exporter.zip_path) if exporter&.zip_path.present? && File.exist?(exporter.zip_path)
  end

  test "sanitizes source reference content and url formatting" do
    article = Article.create!(
      title: "Source Formatting Article",
      slug: "source-formatting-article",
      description: "This is a source formatting article",
      status: :publish,
      content_type: :html,
      html_content: "<p>Source formatting content</p>",
      source_author: "<b>Example Author</b>",
      source_content: "<p>Line one</p><p>Line two<br>Line three</p>",
      source_url: "https://example.com/source\nextra"
    )

    exporter = MarkdownExport.new

    assert exporter.generate, exporter.error_message
    assert exporter.zip_path.present?
    assert File.exist?(exporter.zip_path), "expected zip to exist at #{exporter.zip_path}"

    Zip::File.open(exporter.zip_path) do |zip|
      article_entry = zip.find_entry("articles/source-formatting-article.md")
      assert article_entry, "expected articles/source-formatting-article.md to exist in zip"

      article_content = article_entry.get_input_stream.read
      assert_includes article_content, "Source: Example Author"
      assert_includes article_content, "> Line one"
      assert_includes article_content, "> Line two"
      assert_includes article_content, "> Line three"
      assert_includes article_content, "Original: https://example.com/source"
      refute_includes article_content, "extra"
    end
  ensure
    File.delete(exporter.zip_path) if exporter&.zip_path.present? && File.exist?(exporter.zip_path)
    article&.destroy
  end

  test "skips reference section when source data sanitizes to blank" do
    article = Article.create!(
      title: "Blank Source Article",
      slug: "blank-source-article",
      description: "This is a blank source article",
      status: :publish,
      content_type: :html,
      html_content: "<p>Blank source content</p>",
      source_url: "<br>"
    )

    exporter = MarkdownExport.new

    assert exporter.generate, exporter.error_message
    assert exporter.zip_path.present?
    assert File.exist?(exporter.zip_path), "expected zip to exist at #{exporter.zip_path}"

    Zip::File.open(exporter.zip_path) do |zip|
      article_entry = zip.find_entry("articles/blank-source-article.md")
      assert article_entry, "expected articles/blank-source-article.md to exist in zip"

      article_content = article_entry.get_input_stream.read
      refute_includes article_content, "Reference:"
      assert_includes article_content, "Blank source content"
    end
  ensure
    File.delete(exporter.zip_path) if exporter&.zip_path.present? && File.exist?(exporter.zip_path)
    article&.destroy
  end

  test "uses UTF-8 filenames and markdown image syntax" do
    article = Article.create!(
      title: "中文标题",
      slug: "中文标题",
      description: "desc",
      status: :publish,
      content_type: :html,
      html_content: "<p>正文</p><p><img src=\"attachments/a.png\" alt=\"A\"></p>"
    )

    exporter = MarkdownExport.new
    assert exporter.generate, exporter.error_message

    Zip::File.open(exporter.zip_path) do |zip|
      entry_name = zip.entries.map(&:name).find { |n| n == "articles/中文标题.md" }
      assert entry_name, "expected a UTF-8 article filename containing 中文标题"

      content = zip.find_entry(entry_name).get_input_stream.read
      assert_includes content, "![A](attachments/a.png)"
      refute_includes content, "<img"
    end
  ensure
    File.delete(exporter.zip_path) if exporter&.zip_path.present? && File.exist?(exporter.zip_path)
    article&.destroy
  end
end
