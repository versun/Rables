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

  test "admin create update and destroy page via controller routes" do
    with_routing do |set|
      set.draw do
        resources :pages, param: :slug, only: [ :new, :create, :edit, :update, :destroy ]
        resource :session
        namespace :admin do
          get "/" => "articles#index", as: :root
          resources :pages, only: [ :index ]
        end
      end

      sign_in(@user)

      get new_page_path
      assert_response :not_acceptable

      assert_difference "Page.count", 1 do
        post pages_path, params: {
          page: {
            title: "Created Page",
            slug: "created-page",
            status: "draft",
            content: "Content"
          }
        }
      end
      assert_redirected_to admin_pages_path

      page = Page.find_by!(slug: "created-page")

      patch page_path(page), params: { page: { title: "Updated Page" } }
      assert_redirected_to admin_pages_path
      assert_equal "Updated Page", page.reload.title

      assert_no_difference "Page.count" do
        delete page_path(page)
      end
      assert_equal "trash", page.reload.status
    end
  end

  test "json create failure returns unprocessable" do
    with_routing do |set|
      set.draw do
        resources :pages, param: :slug, only: [ :create ]
        resource :session
        namespace :admin do
          get "/" => "articles#index", as: :root
          resources :pages, only: [ :index ]
        end
      end

      sign_in(@user)

      post pages_path, params: {
        page: { title: "", slug: "" }
      }, as: :json

      assert_response :unprocessable_entity
    end
  end

  test "json update failure returns unprocessable" do
    with_routing do |set|
      set.draw do
        resources :pages, param: :slug, only: [ :update ]
        resource :session
        namespace :admin do
          get "/" => "articles#index", as: :root
          resources :pages, only: [ :index ]
        end
      end

      sign_in(@user)

      page = pages(:published_page)
      patch page_path(page), params: { page: { title: "" } }, as: :json

      assert_response :unprocessable_entity
    end
  end

  test "edit renders and destroy removes trashed page" do
    with_routing do |set|
      set.draw do
        resources :pages, param: :slug, only: [ :edit, :destroy ]
        resource :session
        namespace :admin do
          get "/" => "articles#index", as: :root
          resources :pages, only: [ :index, :edit, :update ]
        end
      end

      sign_in(@user)

      page = Page.create!(
        title: "Trash Page",
        slug: "trash-page-#{Time.current.to_i}",
        status: :trash,
        content: "Content"
      )

      get edit_page_path(page)
      assert_response :success

      assert_difference "Page.count", -1 do
        delete page_path(page)
      end
    end
  end
end
