# frozen_string_literal: true

require "test_helper"

class Admin::ArticlesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    @article = articles(:published_article)
    @draft_article = articles(:draft_article)
    sign_in(@user)
  end

  test "should get index" do
    get admin_articles_path
    assert_response :success
  end

  test "should get new" do
    get new_admin_article_path
    assert_response :success
  end

  test "should get edit" do
    get edit_admin_article_path(@article.slug)
    assert_response :success
  end

  test "should create article" do
    assert_difference "Article.count", 1 do
      post admin_articles_path, params: {
        article: {
          title: "New Admin Article",
          description: "Description",
          status: "draft",
          content_type: "html",
          html_content: "<p>Content</p>"
        }
      }
    end

    assert_redirected_to admin_articles_path
  end

  test "should create article and add another" do
    assert_difference "Article.count", 1 do
      post admin_articles_path, params: {
        article: {
          title: "New Article",
          description: "Description",
          status: "draft",
          content_type: "html",
          html_content: "<p>Content</p>"
        },
        create_and_add_another: "1"
      }
    end

    assert_redirected_to new_admin_article_path
  end

  test "should update article" do
    patch admin_article_path(@article.slug), params: {
      article: {
        title: "Updated Title"
      }
    }

    assert_redirected_to admin_articles_path
    @article.reload
    assert_equal "Updated Title", @article.title
  end

  test "should get drafts" do
    get drafts_admin_articles_path
    assert_response :success
  end

  test "should get scheduled" do
    get scheduled_admin_articles_path
    assert_response :success
  end

  test "should publish article" do
    patch publish_admin_article_path(@draft_article.slug)

    assert_redirected_to admin_articles_path
    @draft_article.reload
    assert @draft_article.publish?
  end

  test "should unpublish article" do
    patch unpublish_admin_article_path(@article.slug)

    assert_redirected_to admin_articles_path
    @article.reload
    assert @article.draft?
  end

  test "should batch add tags" do
    post batch_add_tags_admin_articles_path, params: {
      ids: [ @article.slug ],
      tag_names: "ruby, rails"
    }

    assert_redirected_to admin_articles_path
    @article.reload
    tag_names = @article.tags.pluck(:name).map(&:downcase)
    assert tag_names.include?("ruby"), "Tags should include 'ruby', got: #{tag_names}"
  end

  test "should not batch add tags without ids" do
    post batch_add_tags_admin_articles_path, params: {
      ids: [],
      tag_names: "ruby"
    }

    assert_redirected_to admin_articles_path
  end

  test "should not batch add tags without tag names" do
    post batch_add_tags_admin_articles_path, params: {
      ids: [ @article.slug ],
      tag_names: ""
    }

    assert_redirected_to admin_articles_path
  end

  test "should batch destroy articles - move to trash" do
    # Create a fresh article that's not in trash
    article = Article.create!(
      title: "Article to trash",
      slug: "article-to-trash-#{Time.current.to_i}",
      status: :publish,
      content_type: :html,
      html_content: "<p>Content</p>"
    )

    post batch_destroy_admin_articles_path, params: {
      ids: [ article.slug ]
    }

    assert_redirected_to admin_articles_path
    article.reload
    assert_equal "trash", article.status
  end

  test "should permanently delete trashed articles in batch" do
    trash_article = articles(:trash_article)

    assert_difference "Article.count", -1 do
      post batch_destroy_admin_articles_path, params: {
        ids: [ trash_article.slug ]
      }
    end
  end

  test "should fetch comments" do
    # This test would require mocking the social media services
    # For now, we'll test the basic structure
    skip "Requires social media service mocking"
  end

  test "should batch crosspost with enabled platforms" do
    crosspost = crossposts(:mastodon)
    crosspost.update!(enabled: true)

    article = articles(:published_article)

    assert_enqueued_with(job: CrosspostArticleJob, args: [ article.id, "mastodon" ]) do
      post batch_crosspost_admin_articles_path, params: {
        ids: [ article.slug ],
        platforms: [ "mastodon" ]
      }
    end

    assert_redirected_to admin_articles_path
    assert crosspost.reload.enabled?
  end

  test "should batch newsletter enqueue for published articles" do
    NewsletterSetting.first_or_create.update!(
      enabled: true,
      provider: "native",
      smtp_address: "smtp.example.com",
      smtp_port: 587,
      smtp_user_name: "user",
      smtp_password: "password",
      from_email: "noreply@example.com"
    )

    article = articles(:published_article)

    assert_enqueued_with(job: NativeNewsletterSenderJob, args: [ article.id ]) do
      post batch_newsletter_admin_articles_path, params: { ids: [ article.slug ] }
    end

    assert_redirected_to admin_articles_path
  end

  test "fetch comments creates external comments and parents" do
    article = articles(:published_article)
    SocialMediaPost.create!(article: article, platform: "mastodon", url: "https://mastodon.example/post/1")

    comments_payload = {
      comments: [
        {
          external_id: "c1",
          author_name: "Alice",
          author_username: "alice",
          content: "First",
          published_at: Time.current,
          url: "https://mastodon.example/post/1#c1"
        },
        {
          external_id: "c2",
          author_name: "Bob",
          author_username: "bob",
          content: "Reply",
          published_at: Time.current,
          url: "https://mastodon.example/post/1#c2",
          parent_external_id: "c1"
        }
      ]
    }

    existing_count = article.comments.where(platform: "mastodon").count

    with_stubbed_fetch_comments(MastodonService, comments_payload) do
      post fetch_comments_admin_article_path(article.slug), as: :json
      assert_response :success
      assert_equal true, JSON.parse(response.body)["success"]
    end

    created = article.comments.where(platform: "mastodon")
    assert_equal existing_count + 2, created.count
    parent = created.find_by!(external_id: "c1")
    child = created.find_by!(external_id: "c2")
    assert_equal parent.id, child.parent_id
  end

  test "batch crosspost validates input" do
    post batch_crosspost_admin_articles_path, params: { ids: [], platforms: [ "mastodon" ] }
    assert_redirected_to admin_articles_path

    post batch_crosspost_admin_articles_path, params: { ids: [ @article.slug ], platforms: [] }
    assert_redirected_to admin_articles_path
  end

  test "batch newsletter validates input and skips drafts" do
    post batch_newsletter_admin_articles_path, params: { ids: [] }
    assert_redirected_to admin_articles_path

    draft = articles(:draft_article)
    post batch_newsletter_admin_articles_path, params: { ids: [ draft.slug ] }
    assert_redirected_to admin_articles_path
  end

  private

  def with_stubbed_fetch_comments(service_class, payload)
    original = service_class.instance_method(:fetch_comments)
    service_class.define_method(:fetch_comments) { |_url| payload }
    yield
  ensure
    service_class.define_method(:fetch_comments, original)
  end
end
