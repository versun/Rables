# frozen_string_literal: true

require "test_helper"

class ApplicationJsTest < ActiveSupport::TestCase
  test "assigns window.hljs from the highlight.js module export" do
    source = File.read(Rails.root.join("app/javascript/application.js"))

    assert_includes source, 'import hljs from "highlight.js"'
    assert_includes source, "window.hljs = hljs"
  end

  test "highlights once on turbo:load without a separate DOMContentLoaded pass" do
    source = File.read(Rails.root.join("app/javascript/application.js"))

    assert_includes source, 'document.addEventListener("turbo:load", highlightAll)'
    refute_match(/addEventListener\(\s*["']DOMContentLoaded/, source)
  end

  test "vendors highlight.js instead of pinning a CDN url" do
    importmap = File.read(Rails.root.join("config/importmap.rb"))
    pin_line = importmap.lines.find { |line| line.include?('pin "highlight.js"') }

    assert pin_line, "expected a highlight.js pin in config/importmap.rb"
    refute_includes pin_line, "http"
    assert File.exist?(Rails.root.join("vendor/javascript/highlight.js")),
      "expected vendor/javascript/highlight.js to exist"
  end
end
