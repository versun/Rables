# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class TwitterServiceTest < ActiveSupport::TestCase
  test "verify fails fast when required fields are blank" do
    service = TwitterService.new
    result = service.verify({})

    assert_equal false, result[:success]
    assert_match "Please fill in all information", result[:error]
  end

  test "post returns nil when crosspost is disabled" do
    Crosspost.twitter.update!(enabled: false)
    service = TwitterService.new

    assert_nil service.post(create_published_article)
  end

  test "post uses quote_tweet_id when source_url is x.com" do
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article(source_url: "https://x.com/example/status/1234567890")

    client = Minitest::Mock.new
    client.expect(:get, { "data" => { "username" => "tester" } }, [ "users/me" ])
    client.expect(:post, { "data" => { "id" => "999" } }) do |endpoint, body|
      assert_equal "tweets", endpoint

      payload = JSON.parse(body)
      assert_equal "1234567890", payload["quote_tweet_id"]
      refute_includes payload["text"], article.source_url
      true
    end

    service = TwitterService.new
    result = service.stub(:create_client, client) { service.post(article) }

    assert_equal "https://x.com/tester/status/999", result
    client.verify
  end
end
