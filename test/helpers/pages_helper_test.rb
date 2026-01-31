# frozen_string_literal: true

require "test_helper"

class PagesHelperTest < ActionView::TestCase
  test "page link helpers handle redirect and non-redirect pages" do
    standard_page = pages(:published_page)

    assert_equal page_path(standard_page.slug), page_link_path(standard_page)
    assert_equal({}, page_link_attributes(standard_page))

    redirect_page = Page.create!(
      title: "Redirect Page",
      slug: "redirect-page",
      status: :publish,
      content_type: :html,
      html_content: "<p>Redirect content</p>",
      redirect_url: "https://example.com/redirect"
    )

    assert_equal redirect_page.redirect_url, page_link_path(redirect_page)
    assert_equal({ target: "_blank", rel: "noopener" }, page_link_attributes(redirect_page))
  end
end
