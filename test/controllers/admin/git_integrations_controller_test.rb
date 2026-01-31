# frozen_string_literal: true

require "test_helper"
require "ipaddr"

class Admin::GitIntegrationsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "index update verify and helpers" do
    get admin_git_integrations_path
    assert_response :success
    assert_select ".status-tab.active", text: "GitHub"

    get admin_git_integrations_path(provider: "gitlab")
    assert_response :success
    assert_select ".status-tab.active", text: "GitLab"

    patch admin_git_integration_path("unknown"), params: {
      git_integration: { enabled: "1" }
    }
    assert_response :not_found

    patch admin_git_integration_path("github"), params: {
      git_integration: {
        enabled: "0",
        access_token: "",
        server_url: ""
      }
    }
    assert_redirected_to admin_git_integrations_path(provider: "github")

    patch admin_git_integration_path("github"), params: {
      git_integration: {
        enabled: "1",
        access_token: ""
      }
    }
    assert_redirected_to admin_git_integrations_path(provider: "github")

    post verify_admin_git_integration_path("github"), params: {
      git_integration: { access_token: "" }
    }, as: :json
    assert_response :success
    assert_equal "error", JSON.parse(response.body)["status"]

    with_stubbed_test_connection(success: true, message: "ok") do
      post verify_admin_git_integration_path("github"), params: {
        git_integration: { access_token: "token" }
      }, as: :json
      assert_response :success
      assert_equal "success", JSON.parse(response.body)["status"]
    end

    post verify_admin_git_integration_path("unknown"), params: {
      git_integration: { access_token: "token" }
    }, as: :json
    assert_response :not_found

    controller = Admin::GitIntegrationsController.new
    assert_raises(RuntimeError) { controller.send(:validate_outbound_base_url!, "http://localhost") }
    assert_equal "Server URL is not allowed (loopback/link-local/multicast/unspecified)",
                 controller.send(:outbound_ip_disallowed_reason, IPAddr.new("127.0.0.1"))
    resolved = controller.send(:resolved_ips_for_host, "8.8.8.8")
    assert_equal [ IPAddr.new("8.8.8.8") ], resolved
  end

  test "test_connection covers all providers" do
    controller = Admin::GitIntegrationsController.new
    response_body = {
      login: "octo",
      username: "octo",
      display_name: "Octo User"
    }.to_json

    with_stubbed_resolved_ips do
      with_stubbed_http(response_body) do
        github = GitIntegration.new(provider: "github", name: "GitHub", access_token: "token")
        gitlab = GitIntegration.new(provider: "gitlab", name: "GitLab", access_token: "token")
        gitea = GitIntegration.new(provider: "gitea", name: "Gitea", access_token: "token", server_url: "https://gitea.example.com")
        codeberg = GitIntegration.new(provider: "codeberg", name: "Codeberg", access_token: "token")
        bitbucket = GitIntegration.new(provider: "bitbucket", name: "Bitbucket", access_token: "token", username: "bbuser")

        results = {
          github: controller.send(:test_connection, github),
          gitlab: controller.send(:test_connection, gitlab),
          gitea: controller.send(:test_connection, gitea),
          codeberg: controller.send(:test_connection, codeberg),
          bitbucket: controller.send(:test_connection, bitbucket)
        }

        results.each do |name, result|
          assert result[:success], "#{name} failed: #{result.inspect}"
        end
      end
    end
  end

  private

  def with_stubbed_test_connection(result)
    original = Admin::GitIntegrationsController.instance_method(:test_connection)
    Admin::GitIntegrationsController.define_method(:test_connection) { |_integration| result }
    yield
  ensure
    Admin::GitIntegrationsController.define_method(:test_connection, original)
  end

  FakeHttp = Struct.new(:response) do
    def request(_request)
      response
    end
  end

  class FakeSuccess < Net::HTTPSuccess
    def initialize(body)
      super("1.1", "200", "OK")
      @read = true
      @body = body
    end
  end

  def with_stubbed_http(body)
    original_start = Net::HTTP.method(:start)
    Net::HTTP.define_singleton_method(:start) do |_host, _port, **_opts, &block|
      http = FakeHttp.new(FakeSuccess.new(body))
      block ? block.call(http) : http
    end
    yield
  ensure
    Net::HTTP.define_singleton_method(:start, original_start)
  end

  def with_stubbed_resolved_ips
    original = Admin::GitIntegrationsController.instance_method(:resolved_ips_for_host)
    original_reason = Admin::GitIntegrationsController.instance_method(:outbound_ip_disallowed_reason)
    Admin::GitIntegrationsController.define_method(:resolved_ips_for_host) { |_host| [ IPAddr.new("8.8.8.8") ] }
    Admin::GitIntegrationsController.define_method(:outbound_ip_disallowed_reason) { |_ip| nil }
    yield
  ensure
    Admin::GitIntegrationsController.define_method(:resolved_ips_for_host, original)
    Admin::GitIntegrationsController.define_method(:outbound_ip_disallowed_reason, original_reason)
  end
end
