# frozen_string_literal: true

require "test_helper"

class GitIntegrationTest < ActiveSupport::TestCase
  setup do
    # Clear any existing integrations to avoid uniqueness conflicts
    GitIntegration.delete_all
  end

  test "validates provider presence" do
    integration = GitIntegration.new(name: "Test")
    assert_not integration.valid?
    assert_includes integration.errors[:provider], "can't be blank"
  end

  test "validates provider inclusion" do
    integration = GitIntegration.new(provider: "invalid", name: "Test")
    assert_not integration.valid?
    assert_includes integration.errors[:provider], "is not included in the list"
  end

  test "validates provider uniqueness" do
    GitIntegration.create!(provider: "github", name: "GitHub")
    duplicate = GitIntegration.new(provider: "github", name: "GitHub 2")
    assert_not duplicate.valid?
    assert_includes duplicate.errors[:provider], "has already been taken"
  end

  test "validates name presence" do
    integration = GitIntegration.new(provider: "github")
    assert_not integration.valid?
    assert_includes integration.errors[:name], "can't be blank"
  end

  test "validates access_token presence when enabled" do
    integration = GitIntegration.new(provider: "github", name: "GitHub", enabled: true)
    assert_not integration.valid?
    assert_includes integration.errors[:access_token], "can't be blank"
  end

  test "validates username presence for bitbucket when enabled" do
    integration = GitIntegration.new(provider: "bitbucket", name: "Bitbucket", enabled: true, access_token: "app_password")
    assert_not integration.valid?
    assert_includes integration.errors[:username], "can't be blank"
  end

  test "validates server_url presence for gitea when enabled" do
    integration = GitIntegration.new(provider: "gitea", name: "Gitea", enabled: true, access_token: "token")
    assert_not integration.valid?
    assert_includes integration.errors[:server_url], "can't be blank"
  end

  test "does not require server_url for github when enabled" do
    integration = GitIntegration.new(provider: "github", name: "GitHub", enabled: true, access_token: "ghp_token")
    # server_url is not required for github
    assert_not integration.requires_server_url?
  end

  test "display_name returns correct name" do
    integration = GitIntegration.new(provider: "github", name: "GitHub")
    assert_equal "GitHub", integration.display_name
  end

  test "display_name prefers custom name" do
    integration = GitIntegration.new(provider: "github", name: "My GitHub")
    assert_equal "My GitHub", integration.display_name
  end

  test "server_base_url returns default for github" do
    integration = GitIntegration.new(provider: "github", name: "GitHub")
    assert_equal "https://github.com", integration.server_base_url
  end

  test "server_base_url returns custom url for gitea" do
    integration = GitIntegration.new(provider: "gitea", name: "Gitea", server_url: "https://git.example.com")
    assert_equal "https://git.example.com", integration.server_base_url
  end

  test "configured? returns true when properly configured" do
    integration = GitIntegration.new(provider: "github", name: "GitHub", enabled: true, access_token: "ghp_token")
    assert integration.configured?
  end

  test "configured? returns false when disabled" do
    integration = GitIntegration.new(provider: "github", name: "GitHub", enabled: false, access_token: "ghp_token")
    assert_not integration.configured?
  end

  test "configured? returns false when token missing" do
    integration = GitIntegration.new(provider: "github", name: "GitHub", enabled: true)
    assert_not integration.configured?
  end

  test "build_authenticated_url for github with https" do
    integration = GitIntegration.new(provider: "github", name: "GitHub", access_token: "ghp_token")
    url = integration.build_authenticated_url("https://github.com/user/repo.git")
    assert_equal "https://x-access-token:ghp_token@github.com/user/repo.git", url
  end

  test "build_authenticated_url for github with ssh" do
    integration = GitIntegration.new(provider: "github", name: "GitHub", access_token: "ghp_token")
    url = integration.build_authenticated_url("git@github.com:user/repo.git")
    assert_equal "https://x-access-token:ghp_token@github.com/user/repo.git", url
  end

  test "build_authenticated_url for gitlab" do
    integration = GitIntegration.new(provider: "gitlab", name: "GitLab", access_token: "glpat-token")
    url = integration.build_authenticated_url("https://gitlab.com/user/repo.git")
    assert_equal "https://oauth2:glpat-token@gitlab.com/user/repo.git", url
  end

  test "build_authenticated_url for gitea uses username when provided" do
    integration = GitIntegration.new(provider: "gitea", name: "Gitea", username: "alice", access_token: "token")
    url = integration.build_authenticated_url("https://git.example.com/user/repo.git")
    assert_equal "https://alice:token@git.example.com/user/repo.git", url
  end

  test "build_authenticated_url for gitea falls back to token-only when username missing" do
    integration = GitIntegration.new(provider: "gitea", name: "Gitea", access_token: "token")
    url = integration.build_authenticated_url("https://git.example.com/user/repo.git")
    assert_equal "https://token@git.example.com/user/repo.git", url
  end

  test "build_authenticated_url for codeberg uses username when provided" do
    integration = GitIntegration.new(provider: "codeberg", name: "Codeberg", username: "alice", access_token: "token")
    url = integration.build_authenticated_url("https://codeberg.org/user/repo.git")
    assert_equal "https://alice:token@codeberg.org/user/repo.git", url
  end

  test "build_authenticated_url for codeberg falls back to token-only when username missing" do
    integration = GitIntegration.new(provider: "codeberg", name: "Codeberg", access_token: "token")
    url = integration.build_authenticated_url("https://codeberg.org/user/repo.git")
    assert_equal "https://token@codeberg.org/user/repo.git", url
  end

  test "build_authenticated_url for bitbucket uses username and app password" do
    integration = GitIntegration.new(provider: "bitbucket", name: "Bitbucket", username: "alice", access_token: "app_password")
    url = integration.build_authenticated_url("https://bitbucket.org/workspace/repo.git")
    assert_equal "https://alice:app_password@bitbucket.org/workspace/repo.git", url
  end

  test "mask_token masks github pat" do
    integration = GitIntegration.new(provider: "github", name: "GitHub", access_token: "ghp_abc123")
    masked = integration.mask_token("Error with ghp_abc123 token")
    assert_equal "Error with [REDACTED] token", masked
  end

  test "mask_token masks github fine-grained pat" do
    integration = GitIntegration.new(provider: "github", name: "GitHub", access_token: "github_pat_abc123_xyz")
    masked = integration.mask_token("Error with github_pat_abc123_xyz token")
    assert_equal "Error with [REDACTED] token", masked
  end

  test "ensure_all_providers creates all providers" do
    GitIntegration.ensure_all_providers

    assert_equal 5, GitIntegration.count
    assert GitIntegration.exists?(provider: "github")
    assert GitIntegration.exists?(provider: "gitlab")
    assert GitIntegration.exists?(provider: "gitea")
    assert GitIntegration.exists?(provider: "codeberg")
    assert GitIntegration.exists?(provider: "bitbucket")
  end

  test "active_for_deploy returns enabled integration" do
    integration = GitIntegration.create!(provider: "github", name: "GitHub", access_token: "token", enabled: true)

    result = GitIntegration.active_for_deploy("github")
    assert_equal integration, result
  end

  test "active_for_deploy returns nil for disabled integration" do
    GitIntegration.create!(provider: "github", name: "GitHub", access_token: "token", enabled: false)

    result = GitIntegration.active_for_deploy("github")
    assert_nil result
  end
end
