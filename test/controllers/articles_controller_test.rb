# frozen_string_literal: true

require "test_helper"

class ArticlesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @article = articles(:published_article)
    @draft_article = articles(:draft_article)
    @user = users(:admin)
  end

  test "should get index" do
    get articles_path
    assert_response :success
  end

  test "should get index with search query" do
    get articles_path, params: { q: "Published" }
    assert_response :success
  end

  test "should get index as RSS" do
    get articles_path(format: :rss)
    assert_response :success
    assert_equal "application/xml; charset=utf-8", response.content_type
  end

  test "rss uses content fallback title when missing" do
    article = create_published_article(
      title: nil,
      html_content: "<p>12345678901234567890REST</p>"
    )

    get articles_path(format: :rss)
    assert_response :success
    assert_includes response.body, "<title>12345678901234567890</title>"
  end

  test "rss falls back to date when title and content missing" do
    create_published_article(
      title: nil,
      html_content: "<p></p>",
      created_at: Time.zone.local(2020, 1, 2, 3, 4, 5)
    )

    get articles_path(format: :rss)
    assert_response :success
    assert_includes response.body, "<title>2020-01-02</title>"
  end

  test "should get show for published article" do
    get article_path(@article.slug)
    assert_response :success
  end

  test "show uses setting url for article links" do
    Setting.first.update!(url: "https://settings.example.com")
    CacheableSettings.refresh_site_info

    get article_path(@article.slug)
    assert_response :success

    expected_url = "https://settings.example.com#{article_path(@article)}"
    assert_includes response.body, expected_url
  end

  test "should sanitize script tags in html content" do
    article = create_published_article(html_content: "<p>Safe content</p><script>alert('xss')</script>")

    get article_path(article.slug)
    assert_response :success
    assert_match "<p>Safe content</p>", response.body

    article_content = response.body[/<div class="article-content">(.*?)<\/div>/m, 1]
    assert_no_match(/<script>/, article_content) if article_content
  end

  test "should not show draft article to unauthenticated users" do
    get article_path(@draft_article.slug)
    assert_response :not_found
  end

  test "should show draft article to authenticated users" do
    sign_in(@user)
    get article_path(@draft_article.slug)
    assert_response :success
  end

  test "should get new when authenticated via admin" do
    sign_in(@user)
    get new_admin_article_path
    assert_response :success
  end

  test "should redirect to login when accessing admin new without authentication" do
    get new_admin_article_path
    assert_redirected_to new_session_path
  end

  test "should create article when authenticated via admin" do
    sign_in(@user)

    assert_difference "Article.count", 1 do
      post admin_articles_path, params: {
        article: {
          title: "New Article",
          description: "New description",
          status: "draft",
          content: "New content"
        }
      }
    end

    assert_redirected_to admin_articles_path
  end

  test "should not create article with invalid params via admin" do
    sign_in(@user)

    assert_no_difference "Article.count" do
      post admin_articles_path, params: {
        article: {
          title: "",
          status: "draft"
        }
      }
    end
  end

  test "should get edit when authenticated via admin" do
    sign_in(@user)
    get edit_admin_article_path(@article)
    assert_response :success
  end

  test "should update article when authenticated via admin" do
    sign_in(@user)

    patch admin_article_path(@article), params: {
      article: {
        title: "Updated Title",
        description: "Updated description"
      }
    }

    assert_redirected_to admin_articles_path
    @article.reload
    assert_equal "Updated Title", @article.title
  end

  test "should move article to trash when destroying via admin" do
    sign_in(@user)

    assert_no_difference "Article.count" do
      delete admin_article_path(@article)
    end

    @article.reload
    assert_equal "trash", @article.status
  end

  test "should permanently delete article from trash via admin" do
    sign_in(@user)
    trash_article = articles(:trash_article)

    assert_difference "Article.count", -1 do
      delete admin_article_path(trash_article)
    end
  end

  test "should paginate articles" do
    get articles_path, params: { page: 1 }
    assert_response :success
  end

  test "index uses setting url for article links" do
    Setting.first.update!(url: "https://settings.example.com")
    CacheableSettings.refresh_site_info

    get articles_path
    assert_response :success

    expected_url = "https://settings.example.com#{article_path(@article)}"
    expected_date = @article.created_at.strftime("%Y-%m-%d")
    assert_select "article[data-article-id='#{@article.id}'] a[href='#{expected_url}']", minimum: 1
    assert_select "article[data-article-id='#{@article.id}'] .timeline-date a[href='#{expected_url}'] time", text: expected_date
  end

  test "index renders untitled article without wrapping content link" do
    article = create_published_article(
      title: nil,
      description: nil,
      slug: "untitled-article",
      html_content: "<p>Text <a href=\"https://example.com\">report</a></p>"
    )

    CacheableSettings.refresh_site_info
    get articles_path
    assert_response :success

    assert_select "article[data-article-id='#{article.id}']", 1
    assert_select "article[data-article-id='#{article.id}'][data-controller~='clickable-card']", 0
    assert_select "article[data-article-id='#{article.id}'] a.post-content-link", 0
    assert_select "article[data-article-id='#{article.id}'] a[href='https://example.com']", 1
    expected_url = "#{Setting.first.url}#{article_path(article)}"
    expected_date = article.created_at.strftime("%Y-%m-%d")
    assert_select "article[data-article-id='#{article.id}'] .timeline-date a[href='#{expected_url}']", 1
    assert_select "article[data-article-id='#{article.id}'] .timeline-date a[href='#{expected_url}'] time", text: expected_date
    assert_select "article[data-article-id='#{article.id}'] h2.post-title", text: "Open article", count: 0
  end

  test "admin create update and destroy via controller routes" do
    with_routing do |set|
      set.draw do
        resources :articles, param: :slug, only: [ :new, :create, :edit, :update, :destroy ]
        resource :session
        namespace :admin do
          get "/" => "articles#index", as: :root
          resources :articles, path: "posts", only: [ :index ]
        end
      end

      sign_in(@user)

      get new_article_path
      assert_response :not_acceptable

      assert_difference "Article.count", 1 do
        post articles_path, params: {
          article: {
            title: "Created",
            slug: "created-article",
            status: "draft",
            content: "Content"
          }
        }
      end
      assert_redirected_to admin_articles_path

      article = Article.find_by!(slug: "created-article")

      patch article_path(article), params: { article: { title: "Updated" } }
      assert_redirected_to admin_articles_path
      assert_equal "Updated", article.reload.title

      assert_no_difference "Article.count" do
        delete article_path(article)
      end
      assert_equal "trash", article.reload.status
    end
  end

  test "json create failure returns unprocessable" do
    with_routing do |set|
      set.draw do
        resources :articles, param: :slug, only: [ :create ]
        resource :session
        namespace :admin do
          get "/" => "articles#index", as: :root
          resources :articles, path: "posts", only: [ :index ]
        end
      end

      sign_in(@user)

      post articles_path, params: {
        article: { title: "", status: "draft" }
      }, as: :json

      assert_response :unprocessable_entity
    end
  end
end
