# frozen_string_literal: true

require "test_helper"

class NewsletterMailerTest < ActionMailer::TestCase
  test "confirmation_email uses rails_api_url as base url" do
    subscriber = subscribers(:unconfirmed_subscriber)
    site_info = { title: "Frontend", url: "https://frontend.example.com" }

    with_env("RAILS_API_URL" => "https://api.example.com") do
      email = NewsletterMailer.confirmation_email(subscriber, site_info)

      expected_url = Rails.application.routes.url_helpers.confirm_subscription_url(
        token: subscriber.confirmation_token,
        host: "api.example.com",
        protocol: "https"
      )

      assert_includes email.text_part.body.decoded, expected_url
      assert_includes email.html_part.body.decoded, expected_url
      assert_not_includes email.text_part.body.decoded, "frontend.example.com"
      assert_not_includes email.html_part.body.decoded, "frontend.example.com"
    end
  end

  test "confirmation_email accepts rails_api_url without protocol" do
    subscriber = subscribers(:unconfirmed_subscriber)
    site_info = { title: "Frontend", url: "https://frontend.example.com" }

    with_env("RAILS_API_URL" => "api.example.com") do
      email = NewsletterMailer.confirmation_email(subscriber, site_info)

      expected_url = Rails.application.routes.url_helpers.confirm_subscription_url(
        token: subscriber.confirmation_token,
        host: "api.example.com",
        protocol: "https"
      )

      assert_includes email.text_part.body.decoded, expected_url
      assert_includes email.html_part.body.decoded, expected_url
    end
  end

  test "confirmation_email respects rails_api_url path prefix" do
    subscriber = subscribers(:unconfirmed_subscriber)
    site_info = { title: "Frontend", url: "https://frontend.example.com" }

    with_env("RAILS_API_URL" => "https://api.example.com/api") do
      email = NewsletterMailer.confirmation_email(subscriber, site_info)

      expected_url = Rails.application.routes.url_helpers.confirm_subscription_url(
        token: subscriber.confirmation_token,
        host: "api.example.com",
        protocol: "https",
        script_name: "/api"
      )

      assert_includes email.text_part.body.decoded, expected_url
      assert_includes email.html_part.body.decoded, expected_url
    end
  end

  test "confirmation_email subscribe url starts with rails_api_url" do
    subscriber = subscribers(:unconfirmed_subscriber)
    site_info = { title: "Frontend", url: "https://frontend.example.com" }

    with_env("RAILS_API_URL" => "https://api.example.com") do
      email = NewsletterMailer.confirmation_email(subscriber, site_info)

      text_body = email.text_part.body.decoded
      html_body = email.html_part.body.decoded

      # Extract the subscribe URL from the email body
      # The subscribe URL is the confirmation URL
      assert_match %r{https://api\.example\.com/confirm}, text_body,
        "Subscribe URL in text part should start with rails_api_url"
      assert_match %r{https://api\.example\.com/confirm}, html_body,
        "Subscribe URL in html part should start with rails_api_url"

      # Ensure it does NOT use the frontend URL
      assert_not_includes text_body, "frontend.example.com/confirm",
        "Subscribe URL should not use frontend URL"
      assert_not_includes html_body, "frontend.example.com/confirm",
        "Subscribe URL should not use frontend URL"
    end
  end

  test "confirmation_email subscribe url starts with rails_api_url with path prefix" do
    subscriber = subscribers(:unconfirmed_subscriber)
    site_info = { title: "Frontend", url: "https://frontend.example.com" }

    with_env("RAILS_API_URL" => "https://api.example.com/api/v1") do
      email = NewsletterMailer.confirmation_email(subscriber, site_info)

      text_body = email.text_part.body.decoded
      html_body = email.html_part.body.decoded

      # The subscribe URL should include the path prefix
      assert_match %r{https://api\.example\.com/api/v1/confirm}, text_body,
        "Subscribe URL with path prefix should start with rails_api_url including path"
      assert_match %r{https://api\.example\.com/api/v1/confirm}, html_body,
        "Subscribe URL with path prefix should start with rails_api_url including path"
    end
  end

  test "article_email unsubscribe url starts with rails_api_url" do
    article = articles(:published_article)
    subscriber = subscribers(:confirmed_subscriber)
    site_info = { title: "My Blog", url: "https://frontend.example.com" }

    with_env("RAILS_API_URL" => "https://api.example.com") do
      email = NewsletterMailer.article_email(article, subscriber, site_info)

      text_body = email.text_part.body.decoded
      html_body = email.html_part.body.decoded

      # The unsubscribe URL should start with rails_api_url
      assert_match %r{https://api\.example\.com/unsubscribe}, text_body,
        "Unsubscribe URL in text part should start with rails_api_url"
      assert_match %r{https://api\.example\.com/unsubscribe}, html_body,
        "Unsubscribe URL in html part should start with rails_api_url"

      # Ensure it does NOT use the frontend URL
      assert_not_includes text_body, "frontend.example.com/unsubscribe",
        "Unsubscribe URL should not use frontend URL"
      assert_not_includes html_body, "frontend.example.com/unsubscribe",
        "Unsubscribe URL should not use frontend URL"
    end
  end

  test "article_email unsubscribe url starts with rails_api_url with path prefix" do
    article = articles(:published_article)
    subscriber = subscribers(:confirmed_subscriber)
    site_info = { title: "My Blog", url: "https://frontend.example.com" }

    with_env("RAILS_API_URL" => "https://api.example.com/api/v1") do
      email = NewsletterMailer.article_email(article, subscriber, site_info)

      text_body = email.text_part.body.decoded
      html_body = email.html_part.body.decoded

      # The unsubscribe URL should include the path prefix
      assert_match %r{https://api\.example\.com/api/v1/unsubscribe}, text_body,
        "Unsubscribe URL with path prefix should start with rails_api_url including path"
      assert_match %r{https://api\.example\.com/api/v1/unsubscribe}, html_body,
        "Unsubscribe URL with path prefix should start with rails_api_url including path"
    end
  end

  test "article_email includes source reference content" do
    article = articles(:source_article)
    subscriber = subscribers(:confirmed_subscriber)
    site_info = { title: "My Blog", url: "https://frontend.example.com" }

    with_env("RAILS_API_URL" => "https://api.example.com") do
      email = NewsletterMailer.article_email(article, subscriber, site_info)

      text_body = email.text_part.body.decoded
      html_body = email.html_part.body.decoded

      assert_includes html_body, article.source_author
      assert_includes html_body, article.source_content
      assert_includes html_body, article.source_url

      assert_includes text_body, article.source_author
      assert_includes text_body, article.source_content
      assert_includes text_body, article.source_url
    end
  end

  test "article_email uses setting url for article links" do
    article = articles(:published_article)
    subscriber = subscribers(:confirmed_subscriber)
    Setting.first.update!(url: "https://settings.example.com")
    site_info = { title: "My Blog", url: "https://frontend.example.com" }

    with_env("RAILS_API_URL" => "https://api.example.com") do
      email = NewsletterMailer.article_email(article, subscriber, site_info)

      text_body = email.text_part.body.decoded
      html_body = email.html_part.body.decoded

      expected_url = "https://settings.example.com#{Rails.application.routes.url_helpers.article_path(article)}"

      assert_includes text_body, expected_url
      assert_includes html_body, expected_url
      assert_not_includes text_body, "frontend.example.com"
      assert_not_includes html_body, "frontend.example.com"
    end
  end

  test "article_email renders attachment download links with absolute urls" do
    file = file_fixture("sample.txt")
    blob = ActiveStorage::Blob.create_and_upload!(
      io: file.open,
      filename: "sample.txt",
      content_type: "text/plain"
    )
    attachment = ActionText::Attachment.from_attachable(blob)
    token = SecureRandom.hex(4)
    article = Article.create!(
      title: "Attachment Article #{token}",
      slug: "attachment-article-#{token}",
      status: :publish,
      content_type: :rich_text,
      content: "Hello #{attachment.to_html}"
    )
    subscriber = subscribers(:confirmed_subscriber)
    site_info = { title: "My Blog", url: "https://frontend.example.com" }
    Setting.first.update!(url: "https://settings.example.com")

    with_env("RAILS_API_URL" => "https://api.example.com") do
      email = NewsletterMailer.article_email(article, subscriber, site_info)
      html_body = email.html_part.body.decoded

      expected_url = Rails.application.routes.url_helpers.rails_blob_url(
        blob,
        disposition: "attachment",
        host: "settings.example.com",
        protocol: "https"
      )
      assert_includes html_body, expected_url
    end
  end

  private

  def with_env(env)
    previous = {}
    env.each do |key, value|
      previous[key] = ENV[key]
      ENV[key] = value
    end
    yield
  ensure
    previous.each { |key, value| ENV[key] = value }
  end
end
