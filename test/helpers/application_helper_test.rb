# frozen_string_literal: true

require "test_helper"

class ApplicationHelperTest < ActionView::TestCase
  private

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

  test "safe_html_content preserves prism language classes on code blocks" do
    html = '<pre><code class="language-ruby">puts "hello"</code></pre>'
    sanitized = safe_html_content(html)

    assert_includes sanitized, "<pre><code"
    assert_includes sanitized, 'class="language-ruby"'
  end

  test "rails_api_url forces http for localhost in development" do
    with_rails_env("development") do
      with_env("RAILS_API_URL", "https://localhost:3000/") do
        assert_equal "http://localhost:3000", rails_api_url
      end
    end
  end

  test "absolute_og_image prefixes normalized site url for root-relative path" do
    with_stubbed_site_info({ url: "example.com/blog/" }) do
      assert_equal "https://example.com/blog/uploads/og.png", absolute_og_image("/uploads/og.png")
    end
  end

  test "absolute_og_image keeps url with scheme unchanged" do
    with_stubbed_site_info({ url: "example.com" }) do
      assert_equal "https://cdn.other.com/a.png", absolute_og_image("https://cdn.other.com/a.png")
      assert_equal "http://cdn.other.com/a.png", absolute_og_image("http://cdn.other.com/a.png")
    end
  end

  test "absolute_og_image adds leading slash for bare relative path" do
    with_stubbed_site_info({ url: "example.com/" }) do
      assert_equal "https://example.com/uploads/og.png", absolute_og_image("uploads/og.png")
    end
  end

  test "absolute_og_image returns original value when blank or site url missing" do
    with_stubbed_site_info({ url: "" }) do
      assert_equal "", absolute_og_image(nil)
      assert_equal "/uploads/og.png", absolute_og_image("/uploads/og.png")
      assert_equal "uploads/og.png", absolute_og_image("uploads/og.png")
    end
  end
end
