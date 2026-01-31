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
end
