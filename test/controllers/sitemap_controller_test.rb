# frozen_string_literal: true

require "test_helper"

class SitemapControllerTest < ActionDispatch::IntegrationTest
  test "sitemap returns xml" do
    get sitemap_path(format: :xml)
    assert_response :success
    assert_equal "application/xml; charset=utf-8", response.content_type
    assert_includes response.body, "<urlset"
    assert_includes response.body, "xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\""
  end

  test "sitemap root url lastmod uses latest article updated_at" do
    future = 1.day.from_now.change(usec: 0)
    create_published_article(updated_at: future)

    get sitemap_path(format: :xml)
    assert_response :success

    root_entry = response.body[/<url>.*?<\/url>/m]
    assert_includes root_entry, "<loc>http://localhost:3000</loc>"
    assert_includes root_entry, "<lastmod>#{future.strftime("%Y-%m-%d")}</lastmod>"
  end

  test "sitemap normalizes site url with scheme and without trailing slash" do
    Setting.first.update!(url: "example.com/")
    CacheableSettings.refresh_site_info

    get sitemap_path(format: :xml)
    assert_response :success
    assert_includes response.body, "<loc>https://example.com</loc>"
    assert_no_match(%r{<loc>(?!https?://)}, response.body)
  end

  test "sitemap omits url entries when site url is not configured" do
    Setting.first.update_column(:url, "") # bypass presence validation to simulate an unconfigured url
    CacheableSettings.refresh_site_info

    get sitemap_path(format: :xml)
    assert_response :success
    assert_includes response.body, "<urlset"
    assert_not_includes response.body, "<url>"
  end
end
