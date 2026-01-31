# frozen_string_literal: true

require "test_helper"

class ApplicationHelperTest < ActionView::TestCase
  private

  def with_stubbed_request(user_agent)
    request_stub = Struct.new(:user_agent).new(user_agent)

    singleton_class.class_eval do
      alias_method :__original_request, :request
      define_method(:request) { request_stub }
    end

    yield request_stub
  ensure
    singleton_class.class_eval do
      remove_method :request
      alias_method :request, :__original_request
      remove_method :__original_request
    end
  end

  def with_env(key, value)
    original = ENV[key]
    ENV[key] = value
    yield
  ensure
    if original.nil?
      ENV.delete(key)
    else
      ENV[key] = original
    end
  end

  def with_stubbed_site_info(value)
    CacheableSettings.singleton_class.class_eval do
      alias_method :__original_site_info, :site_info
      define_method(:site_info) { value }
    end

    yield
  ensure
    CacheableSettings.singleton_class.class_eval do
      remove_method :site_info
      alias_method :site_info, :__original_site_info
      remove_method :__original_site_info
    end
  end

  def with_rails_env(value)
    Rails.singleton_class.class_eval do
      alias_method :__original_env, :env
      define_method(:env) { ActiveSupport::StringInquirer.new(value) }
    end

    yield
  ensure
    Rails.singleton_class.class_eval do
      remove_method :env
      alias_method :env, :__original_env
      remove_method :__original_env
    end
  end

  public

  test "mobile_device? detects and memoizes mobile user agents" do
    with_stubbed_request("iphone") do |request_stub|
      assert mobile_device?

      request_stub.user_agent = "desktop"
      assert mobile_device?
    end
  end

  test "rails_api_url normalizes env url and adds protocol" do
    with_env("RAILS_API_URL", "example.com/api/") do
      assert_equal "https://example.com/api", rails_api_url
    end
  end

  test "rails_api_url falls back to site url and normalized_site_url adds https" do
    with_env("RAILS_API_URL", nil) do
      with_stubbed_site_info({ url: "example.org/blog/" }) do
        assert_equal "http://example.org/blog", rails_api_url
        assert_equal "https://example.org/blog", normalized_site_url
      end
    end
  end

  test "safe_html_content removes disallowed tags" do
    html = "<p>Hello</p><script>alert('x')</script>"
    sanitized = safe_html_content(html)

    assert_includes sanitized, "<p>Hello</p>"
    refute_includes sanitized, "script"
  end

  test "rails_api_url forces http for localhost in development" do
    with_rails_env("development") do
      with_env("RAILS_API_URL", "https://localhost:3000/") do
        assert_equal "http://localhost:3000", rails_api_url
      end
    end
  end
end
