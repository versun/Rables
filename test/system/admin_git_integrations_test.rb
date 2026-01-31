# frozen_string_literal: true

require "application_system_test_case"

class AdminGitIntegrationsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @github = git_integrations(:github)
    @gitlab = git_integrations(:gitlab)
  end

  test "viewing git integration tabs" do
    sign_in(@user)
    visit admin_git_integrations_path

    assert_text "Git Integrations"
    assert_text @github.display_name

    click_link @gitlab.display_name
    assert_text @gitlab.display_name
    assert_text "Access Token"
  end

  test "updating github integration" do
    sign_in(@user)
    visit admin_git_integrations_path(provider: "github")

    fill_in "Access Token", with: "new_token_123"
    click_button "保存"

    assert_text "设置已更新"
    @github.reload
    assert_equal "new_token_123", @github.access_token
  end
end
