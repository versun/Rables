# frozen_string_literal: true

require "test_helper"

class GitIntegrationsHelperTest < ActionView::TestCase
  test "provider-specific labels, placeholders, and help text" do
    assert_includes provider_help_text("github"), "GitHub"
    assert_includes provider_help_text("gitlab"), "GitLab"
    assert_includes provider_help_text("gitea"), "Gitea"
    assert_includes provider_help_text("codeberg"), "Codeberg"
    assert_includes provider_help_text("bitbucket"), "Bitbucket"
    assert_equal "", provider_help_text("unknown")

    assert_equal "App Password", token_label("bitbucket")
    assert_equal "Access Token", token_label("github")
    assert_equal "Access Token", token_label("unknown")

    assert_equal "Username", username_label("bitbucket")
    assert_equal "Username (Optional)", username_label("gitea")
    assert_equal "Username (Optional)", username_label("codeberg")
    assert_equal "", username_label("github")

    assert_equal "your-bitbucket-username", username_placeholder("bitbucket")
    assert_equal "your-username", username_placeholder("gitea")
    assert_equal "your-username", username_placeholder("codeberg")
    assert_equal "", username_placeholder("github")

    assert_includes token_placeholder("github"), "ghp_"
    assert_includes token_placeholder("gitlab"), "glpat-"
    assert_equal "xxxxxxxxxxxxxxxx", token_placeholder("gitea")
    assert_equal "xxxxxxxxxxxxxxxx", token_placeholder("codeberg")
    assert_equal "xxxx-xxxx-xxxx-xxxx", token_placeholder("bitbucket")
    assert_equal "", token_placeholder("unknown")

    assert_includes token_help_text("github"), "repo"
    assert_includes token_help_text("gitlab"), "write_repository"
    assert_includes token_help_text("gitea"), "repository"
    assert_includes token_help_text("codeberg"), "repository"
    assert_includes token_help_text("bitbucket"), "repository write"
    assert_equal "", token_help_text("unknown")
  end
end
