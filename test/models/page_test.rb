# frozen_string_literal: true

require "test_helper"

class PageTest < ActiveSupport::TestCase
  def setup
    @page = Page.new(
      title: "Test Page",
      slug: "test-page",
      status: :draft,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )
  end

  test "should be valid with valid attributes" do
    assert @page.valid?
  end

  test "should require title" do
    @page.title = nil
    assert_not @page.valid?
  end

  test "should require unique slug" do
    existing_page = Page.new(
      title: "Existing",
      slug: "existing-page",
      status: :draft,
      content_type: :html,
      html_content: "<p>Existing content</p>"
    )
    existing_page.save!

    @page.slug = existing_page.slug
    assert_not @page.valid?
  end

  test "should have status enum" do
    assert_respond_to @page, :status
  end

  test "should have published scope" do
    published_page = Page.new(
      title: "Published Page",
      slug: "published-page",
      status: :publish,
      content_type: :html,
      html_content: "<p>Published content</p>"
    )
    published_page.save!

    draft_page = Page.new(
      title: "Draft Page",
      slug: "draft-page",
      status: :draft,
      content_type: :html,
      html_content: "<p>Draft content</p>"
    )
    draft_page.save!

    published = Page.published
    assert_includes published, published_page
    assert_not_includes published, draft_page
  end

  test "to_param should return slug" do
    @page.slug = "test-slug"
    assert_equal "test-slug", @page.to_param
  end

  test "redirect? should return true when redirect_url is present" do
    @page.redirect_url = "https://example.com"
    assert @page.redirect?
  end

  test "redirect? should return false when redirect_url is blank" do
    assert_not @page.redirect?
  end

  test "should validate redirect_url format" do
    @page.redirect_url = "not-a-url"
    assert_not @page.valid?
  end

  test "should allow blank redirect_url" do
    @page.redirect_url = ""
    assert @page.valid?
  end

  test "rendered_content returns rich text content when rich_text" do
    page = Page.create!(
      title: "Rich Page",
      slug: "rich-page",
      status: :publish,
      content: "<p>Rich content</p>"
    )

    assert_includes page.rendered_content.to_s, "Rich content"
  end
end
