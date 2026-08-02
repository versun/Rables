# frozen_string_literal: true

require "test_helper"

class PagesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @published_page = pages(:published_page)
    @draft_page = pages(:draft_page)
    @shared_page = pages(:shared_page)
    @page_with_script = pages(:page_with_script)
    @user = users(:admin)
  end

  test "should show published page" do
    get page_path("published-page-fixture")
    assert_response :success
    assert_match "Published page content", response.body
  end

  test "should show shared page" do
    get page_path("shared-page-fixture")
    assert_response :success
  end

  test "show uses private cache control so shared caches do not serve per-session csrf tokens" do
    get page_path("published-page-fixture")
    assert_response :success
    assert_includes response.headers["Cache-Control"], "private"
    assert_includes response.headers["Cache-Control"], "max-age=86400"
    assert_not_includes response.headers["Cache-Control"], "public"
  end

  test "show renders giscus only when page allows comments" do
    get page_path("published-page-fixture")
    assert_response :success
    assert_includes response.body, %(<div class="giscus"></div>)

    get page_path("shared-page-fixture")
    assert_response :success
    assert_not_includes response.body, %(<div class="giscus"></div>)
  end

  test "should not show draft page to unauthenticated users" do
    get page_path("draft-page-fixture")
    assert_response :not_found
  end

  test "should show draft page to authenticated users" do
    sign_in(@user)
    get page_path("draft-page-fixture")
    assert_response :success
  end

  test "should return 404 for non-existent page" do
    get page_path("non-existent-page")
    assert_response :not_found
  end

  test "should render html content for html pages" do
    get page_path("published-page-fixture")
    assert_response :success
    assert_match "<p>Published page content</p>", response.body
  end

  test "should sanitize script tags in html content" do
    get page_path("page-with-script-fixture")
    assert_response :success
    # Script tags should be stripped by the sanitizer
    assert_match "Safe content", response.body
    # The script tag should be removed - verify by checking the article content div
    # The malicious script content becomes plain text (not executable)
    assert_match %r{<p>Safe content</p>alert\('xss'\)}, response.body
    # Verify no script tag with alert exists in the article-content
    article_content = response.body[/<div class="article-content">(.*?)<\/div>/m, 1]
    assert_no_match(/<script>/, article_content) if article_content
  end
end
