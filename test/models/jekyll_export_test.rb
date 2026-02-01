# frozen_string_literal: true

require "test_helper"

class JekyllExportTest < ActiveSupport::TestCase
  def setup
    @setting = JekyllSetting.instance
    @exporter = JekyllExport.new(@setting)
    @article = articles(:published_article)
    @page = pages(:published_page)
  end

  test "build_article_front_matter includes required fields" do
    fm = @exporter.build_article_front_matter(@article)

    assert_equal "post", fm["layout"]
    assert_equal @article.title, fm["title"]
    assert_equal @article.slug, fm["slug"]
    assert fm["date"].present?
  end

  test "build_article_front_matter includes tags when present" do
    @article.tags << tags(:ruby)
    fm = @exporter.build_article_front_matter(@article)

    assert_includes fm["tags"], "Ruby"
    assert_includes fm["categories"], "Ruby"
  end

  test "build_article_front_matter includes source reference when present" do
    @article.update(
      source_author: "John Doe",
      source_url: "https://example.com/original"
    )

    fm = @exporter.build_article_front_matter(@article)
    assert_equal "John Doe", fm["source_author"]
    assert_equal "https://example.com/original", fm["source_url"]
  end

  test "build_page_front_matter includes required fields" do
    fm = @exporter.build_page_front_matter(@page)

    assert_equal "page", fm["layout"]
    assert_equal @page.title, fm["title"]
    assert_equal @page.slug, fm["slug"]
  end

  test "build_page_front_matter includes order when present" do
    @page.update(page_order: 5)
    fm = @exporter.build_page_front_matter(@page)

    assert_equal 5, fm["order"]
  end

  test "article_filename includes date prefix" do
    filename = @exporter.article_filename(@article)
    date_prefix = @article.created_at.strftime("%Y-%m-%d")

    assert filename.start_with?(date_prefix)
    assert filename.end_with?(".md")
  end

  test "page_filename is just slug with extension" do
    filename = @exporter.page_filename(@page)
    assert_equal "#{@page.slug}.md", filename
  end

  test "article_to_markdown includes front matter and content" do
    markdown = @exporter.article_to_markdown(@article)

    assert markdown.start_with?("---\n")
    assert_includes markdown, "layout: post"
    assert_includes markdown, @article.title
  end

  test "export_article returns hash with filename and content" do
    exported = @exporter.export_article(@article)

    assert exported[:filename].present?
    assert exported[:content].present?
    assert exported[:front_matter].present?
  end

  test "export_page returns hash with filename and content" do
    exported = @exporter.export_page(@page)

    assert exported[:filename].present?
    assert exported[:content].present?
    assert exported[:front_matter].present?
  end

  test "generate returns articles and pages" do
    result = @exporter.generate(articles: Article.where(id: @article.id), pages: Page.where(id: @page.id))

    assert_equal 1, result[:articles].length
    assert_equal 1, result[:pages].length
  end
end
