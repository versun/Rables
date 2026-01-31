# frozen_string_literal: true

require "simplecov"

if ENV["TEST_ENV_NUMBER"].to_s.empty?
  at_exit do
    resultset_path = File.join(SimpleCov.coverage_path, ".resultset.json")
    SimpleCov.collate([ resultset_path ], "rails") if File.exist?(resultset_path)
  end
end

SimpleCov.start "rails"

process_label = ENV["TEST_ENV_NUMBER"].to_s
process_label = "main" if process_label.empty?
SimpleCov.command_name "minitest-#{process_label}"

SimpleCov.at_exit do
  SimpleCov.result
end
ENV["RAILS_ENV"] ||= "test"
require_relative "../config/environment"
require "rails/test_help"
require "active_job/test_helper"

# Clean up parallel test database files after all tests complete
Minitest.after_run do
  db_dir = Rails.root.join("db")
  parallel_db_patterns = [
    "test.sqlite3_*",
    "test_cache.sqlite3_*",
    "test_queue.sqlite3_*",
    "test_cable.sqlite3_*"
  ]

  parallel_db_patterns.each do |pattern|
    Dir.glob(db_dir.join(pattern)).each do |file|
      File.delete(file) if File.exist?(file)
    end
  end
end

class ActiveSupport::TestCase
  include ActiveJob::TestHelper

  setup do
    clear_enqueued_jobs
    clear_performed_jobs
  end

  # Run tests in parallel with specified workers
  parallelize(workers: :number_of_processors)

  # Setup all fixtures in test/fixtures/*.yml for all tests in alphabetical order.
  fixtures :all

  # Add more helper methods to be used by all tests here...

  # Helper method to create a user with authentication
  def create_user(user_name: "testuser", password: "password123")
    User.create!(
      user_name: user_name,
      password: password,
      password_confirmation: password
    )
  end

  # Helper method to create a published article
  def create_published_article(attributes = {})
    default_attributes = {
      title: "Test Article #{Time.current.to_i}-#{rand(10000)}",
      slug: "test-article-#{Time.current.to_i}-#{rand(10000)}",
      description: "Test description",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>"
    }
    Article.create!(default_attributes.merge(attributes))
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

class ActionDispatch::IntegrationTest
  include ActiveJob::TestHelper

  setup do
    clear_enqueued_jobs
    clear_performed_jobs
  end

  # Helper method to sign in a user for integration tests
  def sign_in(user)
    post session_path, params: {
      user_name: user.user_name,
      password: "password123"
    }
    user
  end

  # Helper method to create a published article
  def create_published_article(attributes = {})
    default_attributes = {
      title: "Test Article #{Time.current.to_i}-#{rand(10000)}",
      slug: "test-article-#{Time.current.to_i}-#{rand(10000)}",
      description: "Test description",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>"
    }
    Article.create!(default_attributes.merge(attributes))
  end

  # Helper method to create a draft article
  def create_draft_article(attributes = {})
    default_attributes = {
      title: "Draft Article #{Time.current.to_i}-#{rand(10000)}",
      slug: "draft-article-#{Time.current.to_i}-#{rand(10000)}",
      description: "Draft description",
      status: :draft,
      content_type: :html,
      html_content: "<p>Draft content</p>"
    }
    Article.create!(default_attributes.merge(attributes))
  end

  # Helper method to create a tag
  def create_tag(name: "test-tag", slug: nil)
    Tag.create!(
      name: name,
      slug: slug || name.parameterize
    )
  end

  # Helper method to create a subscriber
  def create_subscriber(email: "test@example.com", confirmed: true)
    subscriber = Subscriber.create!(
      email: email,
      confirmation_token: SecureRandom.urlsafe_base64(32),
      unsubscribe_token: SecureRandom.urlsafe_base64(32)
    )
    subscriber.update!(confirmed_at: Time.current) if confirmed
    subscriber
  end
end
