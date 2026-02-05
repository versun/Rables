# frozen_string_literal: true

require "test_helper"
require "rack/mock"

class RedirectMiddlewareTest < ActiveSupport::TestCase
  test "skips admin paths" do
    app = ->(_env) { [ 200, { "Content-Type" => "text/plain" }, [ "ok" ] ] }
    middleware = RedirectMiddleware.new(app)

    status, _headers, body = middleware.call(Rack::MockRequest.env_for("/admin/dashboard"))

    assert_equal 200, status
    assert_equal "ok", body.join
  end

  test "applies permanent redirect" do
    Redirect.create!(regex: "^/old$", replacement: "/new", enabled: true, permanent: true)

    app = ->(_env) { [ 200, { "Content-Type" => "text/plain" }, [ "ok" ] ] }
    middleware = RedirectMiddleware.new(app)

    status, headers, _body = middleware.call(Rack::MockRequest.env_for("/old"))

    assert_equal 301, status
    assert_equal "/new", headers["Location"]
  end

  test "applies temporary redirect" do
    Redirect.create!(regex: "^/temp$", replacement: "/target", enabled: true, permanent: false)

    app = ->(_env) { [ 200, { "Content-Type" => "text/plain" }, [ "ok" ] ] }
    middleware = RedirectMiddleware.new(app)

    status, headers, _body = middleware.call(Rack::MockRequest.env_for("/temp"))

    assert_equal 302, status
    assert_equal "/target", headers["Location"]
  end
end
