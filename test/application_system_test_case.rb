# frozen_string_literal: true

require "test_helper"
require "shellwords"

class ApplicationSystemTestCase < ActionDispatch::SystemTestCase
  def self.system_test_driver
    (ENV["SYSTEM_TEST_DRIVER"] || "").strip
  end

  def self.executable_available?(command_or_path)
    return false if command_or_path.nil? || command_or_path.strip.empty?

    if command_or_path.include?(File::SEPARATOR)
      File.exist?(command_or_path) && File.executable?(command_or_path)
    else
      system("command -v #{Shellwords.shellescape(command_or_path)} >/dev/null 2>&1")
    end
  end

  def self.chrome_available?
    [
      ENV["CHROME_BIN"],
      "google-chrome",
      "google-chrome-stable",
      "chromium",
      "chromium-browser"
    ].compact.any? { |candidate| executable_available?(candidate) }
  end

  def self.use_selenium?
    return true if system_test_driver == "selenium"
    return false if system_test_driver == "rack_test"

    chrome_available?
  end

  if use_selenium?
    driven_by :selenium, using: :headless_chrome, screen_size: [ 1400, 1400 ]
  else
    driven_by :rack_test
  end

  # Helper method to sign in a user in system tests
  def sign_in(user)
    visit new_session_path
    fill_in "user_name", with: user.user_name
    fill_in "password", with: "password123"
    click_button "Sign in"
  end

  # Helper method to create a published article
  def create_published_article(attributes = {})
    default_attributes = {
      title: "Test Article #{Time.current.to_i}-#{rand(10000)}",
      slug: "test-article-#{Time.current.to_i}-#{rand(10000)}",
      description: "Test description",
      status: :publish,
      content_type: :html,
      html_content: attributes[:content] || "<p>Test content</p>"
    }
    Article.create!(default_attributes.merge(attributes.except(:content)))
  end

  # Helper method to create a draft article
  def create_draft_article(attributes = {})
    default_attributes = {
      title: "Draft Article #{Time.current.to_i}-#{rand(10000)}",
      slug: "draft-article-#{Time.current.to_i}-#{rand(10000)}",
      description: "Draft description",
      status: :draft,
      content_type: :html,
      html_content: attributes[:content] || "<p>Draft content</p>"
    }
    Article.create!(default_attributes.merge(attributes.except(:content)))
  end

  # Helper method to create a tag
  def create_tag(name: "test-tag", slug: nil)
    Tag.create!(
      name: name,
      slug: slug || name.parameterize
    )
  end

  # Helper method to create a subscriber
  def create_subscriber(email: "test#{Time.current.to_i}#{rand(10000)}@example.com", confirmed: true)
    subscriber = Subscriber.create!(
      email: email,
      confirmation_token: SecureRandom.urlsafe_base64(32),
      unsubscribe_token: SecureRandom.urlsafe_base64(32)
    )
    subscriber.update!(confirmed_at: Time.current) if confirmed
    subscriber
  end
end
