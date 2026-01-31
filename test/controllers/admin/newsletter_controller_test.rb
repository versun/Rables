# frozen_string_literal: true

require "test_helper"

class Admin::NewsletterControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "update accepts submit param and redirects" do
    patch admin_newsletter_path, params: {
      tab: "native",
      newsletter_setting: {
        enabled: "1",
        provider: "native",
        smtp_address: "smtp.example.com",
        smtp_port: "587",
        smtp_user_name: "user",
        smtp_password: "password",
        smtp_domain: "example.com",
        smtp_authentication: "plain",
        smtp_enable_starttls: "1",
        from_email: "noreply@example.com",
        footer: "<p><br></p>",
        submit: "Save Native Email Settings"
      }
    }

    assert_redirected_to admin_newsletter_path(tab: "native")
    assert_equal "smtp.example.com", NewsletterSetting.first.smtp_address
  end

  test "update renders show on validation failure without error" do
    patch admin_newsletter_path, params: {
      tab: "native",
      newsletter_setting: {
        enabled: "1",
        provider: "native",
        smtp_address: "smtp.example.com",
        smtp_port: "587",
        smtp_user_name: "user",
        smtp_password: "password",
        smtp_domain: "example.com",
        smtp_authentication: "plain",
        smtp_enable_starttls: "1",
        from_email: "invalid-email",
        footer: "<p><br></p>",
        submit: "Save Native Email Settings"
      }
    }

    assert_response :unprocessable_entity
    assert_select "h3", text: "Newsletter Settings"
  end

  test "listmonk update failure renders show without error" do
    patch admin_newsletter_path, params: {
      tab: "listmonk",
      listmonk: {
        url: "not-a-url",
        username: "",
        api_key: ""
      }
    }

    assert_response :unprocessable_entity
    assert_select "h3", text: "Newsletter Settings"
    assert_select "select#list-select"
    assert_select "select#template-select"
  end

  test "verify listmonk responses and update listmonk settings" do
    get admin_newsletter_path
    assert_response :success
    assert_select ".status-tab.active", text: "General"

    get admin_newsletter_path(tab: "listmonk")
    assert_response :success
    assert_select ".status-tab.active", text: "Listmonk"

    post verify_admin_newsletter_path, params: { username: "", api_key: "", url: "" }, as: :json
    assert_response :unprocessable_entity
    assert_equal false, JSON.parse(response.body)["success"]

    ActivityLog.create!(
      action: "failed",
      target: "newsletter",
      level: :error,
      description: "Listmonk fetch failed"
    )

    with_stubbed_listmonk(configured: true, lists: [], templates: []) do
      post verify_admin_newsletter_path, params: {
        username: "user",
        api_key: "key",
        url: "https://listmonk.example"
      }, as: :json
      assert_response :unprocessable_entity
      assert_equal false, JSON.parse(response.body)["success"]
    end

    with_stubbed_listmonk(configured: true, lists: [ { "id" => 1 } ], templates: [ { "id" => 2 } ]) do
      post verify_admin_newsletter_path, params: {
        username: "user",
        api_key: "key",
        url: "https://listmonk.example",
        list_id: 1,
        template_id: 2
      }, as: :json
      assert_response :success
      assert_equal true, JSON.parse(response.body)["success"]
    end

    patch admin_newsletter_path, params: {
      tab: "listmonk",
      listmonk: {
        enabled: "1",
        url: "https://listmonk.example",
        username: "user",
        api_key: "key",
        list_id: 1,
        template_id: 2
      }
    }
    assert_redirected_to admin_newsletter_path(tab: "listmonk")
  end

  test "verify smtp handles validation and authentication errors" do
    post verify_admin_newsletter_path, params: { smtp_address: "", smtp_user_name: "" }, as: :json
    assert_response :unprocessable_entity
    assert_equal false, JSON.parse(response.body)["success"]

    smtp_params = {
      smtp_address: "smtp.example.com",
      smtp_port: "587",
      smtp_user_name: "user",
      smtp_password: "secret",
      smtp_domain: "example.com",
      smtp_authentication: "plain",
      smtp_enable_starttls: "1",
      from_email: "noreply@example.com"
    }

    with_stubbed_smtp(behavior: :success) do
      post verify_admin_newsletter_path, params: smtp_params, as: :json
      assert_response :success
      assert_equal true, JSON.parse(response.body)["success"]
    end

    with_stubbed_smtp(behavior: :auth_error) do
      post verify_admin_newsletter_path, params: smtp_params, as: :json
      assert_response :unprocessable_entity
      assert_equal false, JSON.parse(response.body)["success"]
    end
  end

  private

  def with_stubbed_listmonk(configured:, lists:, templates:)
    original_configured = Listmonk.instance_method(:configured?)
    original_fetch_lists = Listmonk.instance_method(:fetch_lists)
    original_fetch_templates = Listmonk.instance_method(:fetch_templates)

    Listmonk.define_method(:configured?) { configured }
    Listmonk.define_method(:fetch_lists) { lists }
    Listmonk.define_method(:fetch_templates) { templates }
    yield
  ensure
    Listmonk.define_method(:configured?, original_configured)
    Listmonk.define_method(:fetch_lists, original_fetch_lists)
    Listmonk.define_method(:fetch_templates, original_fetch_templates)
  end

  def with_stubbed_smtp(behavior:)
    original_new = Net::SMTP.method(:new)
    fake_class = Class.new do
      attr_accessor :open_timeout, :read_timeout

      def initialize(behavior)
        @behavior = behavior
      end

      def enable_starttls; end

      def start(*_args)
        case @behavior
        when :success
          yield if block_given?
        when :auth_error
          raise Net::SMTPAuthenticationError.new("Invalid credentials")
        else
          raise Timeout::Error, "timeout"
        end
      end
    end

    Net::SMTP.define_singleton_method(:new) { |_address, _port| fake_class.new(behavior) }
    yield
  ensure
    Net::SMTP.define_singleton_method(:new, original_new)
  end
end
