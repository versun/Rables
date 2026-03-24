# frozen_string_literal: true

require "test_helper"

class ApplicationStylesheetTest < ActiveSupport::TestCase
  LIGHT_INLINE_CODE_RULE = /
    \.article-content\s*>\s*code:not\(\[class\*="language-"\]\),
    \s*\.post-content\s*>\s*code:not\(\[class\*="language-"\]\),
    \s*\.article-content\s+:not\(pre\)\s*>\s*code:not\(\[class\*="language-"\]\),
    \s*\.post-content\s+:not\(pre\)\s*>\s*code:not\(\[class\*="language-"\]\)
    \s*\{
    (?<body>.*?)
    \}
  /mx

  DARK_INLINE_CODE_RULE = /
    \[data-theme="dark"\]\s+\.article-content\s*>\s*code:not\(\[class\*="language-"\]\),
    \s*\[data-theme="dark"\]\s+\.post-content\s*>\s*code:not\(\[class\*="language-"\]\),
    \s*\[data-theme="dark"\]\s+\.article-content\s+:not\(pre\)\s*>\s*code:not\(\[class\*="language-"\]\),
    \s*\[data-theme="dark"\]\s+\.post-content\s+:not\(pre\)\s*>\s*code:not\(\[class\*="language-"\]\)
    \s*\{
    (?<body>.*?)
    \}
  /mx

  test "inline code light theme styles include root-level code nodes" do
    stylesheet = File.read(Rails.root.join("app/assets/stylesheets/application.css"))
    rule_body = stylesheet[LIGHT_INLINE_CODE_RULE, :body]

    refute_nil rule_body, "expected application.css to style root-level and nested inline code outside pre blocks"
    assert_includes rule_body, "background-color: #f6f8fa;"
    assert_includes rule_body, "border: 1px solid #d0d7de;"
    assert_includes rule_body, "border-radius: 4px;"
    assert_includes rule_body, "padding: 0.15em 0.35em;"
  end

  test "inline code dark theme styles include root-level code nodes" do
    stylesheet = File.read(Rails.root.join("app/assets/stylesheets/application.css"))
    rule_body = stylesheet[DARK_INLINE_CODE_RULE, :body]

    refute_nil rule_body, "expected application.css dark theme to style root-level and nested inline code outside pre blocks"
    assert_includes rule_body, "background-color: #2a2a2a;"
    assert_includes rule_body, "border: 1px solid var(--border-color);"
  end
end
