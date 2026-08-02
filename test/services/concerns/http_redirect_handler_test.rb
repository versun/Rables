# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class HttpRedirectHandlerTest < ActiveSupport::TestCase
  class DummyClient
    include HttpRedirectHandler
  end

  setup do
    @client = DummyClient.new
  end

  test "fetch_with_redirect refuses non-http(s) urls" do
    error = assert_raises(RuntimeError) do
      @client.fetch_with_redirect(URI("file:///etc/passwd"))
    end

    assert_includes error.message, "non-http(s)"
  end

  test "fetch_with_redirect refuses redirects to non-http(s) schemes" do
    redirect = build_response(Net::HTTPRedirection, "302", "Found", { "location" => "file:///etc/passwd" })

    error = assert_raises(RuntimeError) do
      with_stubbed_http([ redirect ]) do
        @client.fetch_with_redirect(URI("https://example.com/start"))
      end
    end

    assert_includes error.message, "non-http(s)"
  end

  test "fetch_with_redirect refuses https to http downgrades" do
    redirect = build_response(Net::HTTPRedirection, "302", "Found", { "location" => "http://example.com/insecure" })

    error = assert_raises(RuntimeError) do
      with_stubbed_http([ redirect ]) do
        @client.fetch_with_redirect(URI("https://example.com/start"))
      end
    end

    assert_includes error.message, "downgrades https to http"
  end

  test "fetch_with_redirect follows http to https upgrades" do
    redirect = build_response(Net::HTTPRedirection, "301", "Moved", { "location" => "https://example.com/final" })
    success = build_response(Net::HTTPSuccess, "200", "OK", { "content-type" => "image/png" }, body: "image-data")

    response = nil
    with_stubbed_http([ redirect, success ]) do
      response = @client.fetch_with_redirect(URI("http://example.com/start"))
    end

    assert_equal "image-data", response.body
  end

  test "download_remote_image_with_redirect returns nil when content-length exceeds the cap" do
    response = build_response(Net::HTTPSuccess, "200", "OK",
      { "content-type" => "image/png", "content-length" => (HttpRedirectHandler::MAX_DOWNLOAD_BYTES + 1).to_s },
      body: "small")

    @client.stub(:fetch_with_redirect, response) do
      assert_nil @client.download_remote_image_with_redirect("http://example.com/huge.png")
    end
  end

  test "download_remote_image_with_redirect returns nil when the body exceeds the cap" do
    response = build_response(Net::HTTPSuccess, "200", "OK",
      { "content-type" => "image/png" },
      body: "a" * (HttpRedirectHandler::MAX_DOWNLOAD_BYTES + 1))

    @client.stub(:fetch_with_redirect, response) do
      assert_nil @client.download_remote_image_with_redirect("http://example.com/huge.png")
    end
  end

  test "download_remote_image_with_redirect accepts responses within the cap" do
    response = build_response(Net::HTTPSuccess, "200", "OK",
      { "content-type" => "image/png", "content-length" => "9" },
      body: "image-data")

    @client.stub(:fetch_with_redirect, response) do
      data, content_type = @client.download_remote_image_with_redirect("http://example.com/ok.png")

      assert_equal "image-data", data
      assert_equal "image/png", content_type
    end
  end

  private

  def build_response(klass, code, message, headers = {}, body: nil)
    response = klass.new("1.1", code, message)
    headers.each { |key, value| response[key] = value }
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, body)
    response
  end

  # Serves the queued responses through a fake Net::HTTP, one per request
  def with_stubbed_http(responses)
    factory = ->(*_args) { build_fake_http(responses.shift) }
    Net::HTTP.stub(:new, factory) { yield }
  end

  def build_fake_http(response)
    http = Object.new
    http.define_singleton_method(:use_ssl=) { |_| }
    http.define_singleton_method(:open_timeout=) { |_| }
    http.define_singleton_method(:read_timeout=) { |_| }
    http.define_singleton_method(:request) { |_| response }
    http
  end
end
