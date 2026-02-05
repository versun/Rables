# frozen_string_literal: true

require "test_helper"

class BlueskyServiceTest < ActiveSupport::TestCase
  test "verify fails fast when credentials are blank" do
    service = BlueskyService.new
    result = service.verify({})

    assert_equal false, result[:success]
    assert_match "App Password", result[:error]
  end

  test "post returns nil when crosspost is disabled" do
    Crosspost.bluesky.update!(enabled: false)
    service = BlueskyService.new

    assert_nil service.post(create_published_article)
  end

  test "post uploads all images for bluesky" do
    Crosspost.bluesky.update!(
      enabled: true,
      username: "tester",
      app_password: "app-password"
    )
    service = BlueskyService.new
    article = create_published_article

    images = [ Object.new, Object.new, Object.new ]
    article.define_singleton_method(:all_image_attachments) { |_limit| images }

    captured_images = nil
    embed_payload = { "$type" => "app.bsky.embed.images", "images" => [] }
    service.define_singleton_method(:upload_images_embed) do |attachables|
      captured_images = attachables
      embed_payload
    end

    captured_embed = nil
    service.define_singleton_method(:skeet) do |_message, embed = nil|
      captured_embed = embed
      "https://bsky.app/profile/tester/post/123"
    end

    result = service.post(article)

    assert_equal images, captured_images
    assert_equal embed_payload, captured_embed
    assert_equal "https://bsky.app/profile/tester/post/123", result
  end
end
