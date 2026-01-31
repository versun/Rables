# frozen_string_literal: true

require "application_system_test_case"

class AdminArticlesTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
  end

  test "publishing an article" do
    article = create_draft_article(title: "Admin Flow Draft", content: "Draft body")

    sign_in(@user)

    visit edit_admin_article_path(article)
    select "publish", from: "status_select"
    click_button "Save"

    assert_text "Article was successfully updated."
    article.reload
    assert article.publish?
  end

  test "creating a new html article" do
    sign_in(@user)
    visit new_admin_article_path

    select "HTML Code", from: "content_type_select"
    fill_in "Title", with: "New Admin Article"
    fill_in "Slug", with: "new-admin-article"
    fill_in "Description", with: "Admin description"
    fill_in "article[html_content]", with: "<p>HTML content</p>", visible: :all
    select "draft", from: "status_select"
    click_button "Save"

    assert_text "Article was successfully created."
    assert Article.exists?(slug: "new-admin-article")
  end

  test "filtering articles by status" do
    published_article = create_published_article(title: "Published Filter", content: "Body")
    create_draft_article(title: "Draft Filter", content: "Draft")

    sign_in(@user)
    visit admin_articles_path(status: "publish")

    assert_text published_article.title
    assert_no_text "Draft Filter"
  end

  test "trashing an article" do
    skip "This test requires JavaScript support (Selenium)" unless self.class.use_selenium?

    article = create_draft_article(title: "Article to Trash", content: "Draft body")

    sign_in(@user)

    visit admin_articles_path
    find("tr", text: article.title).find("a[title='Trash']").click

    assert_text "Article was successfully moved to trash."

    article.reload
    assert article.trash?
  end

  test "admin list hides unsupported social platforms" do
    article = create_draft_article(title: "Admin List Platform Filter", content: "Draft body")
    article.social_media_posts.create!(platform: "mastodon", url: "https://mastodon.social/@test/2")
    article.social_media_posts.create!(platform: "internet_archive", url: "https://web.archive.org/web/123/http://example.com")

    sign_in(@user)
    visit admin_articles_path

    within find("tr", text: article.title) do
      assert_selector "[data-fetch-comments-platform-value='mastodon']"
      assert_no_selector "[data-fetch-comments-platform-value='internet_archive']"
    end
  end
end
