# frozen_string_literal: true

require "application_system_test_case"

class AdminRedirectsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @redirect = Redirect.create!(regex: "^/old$", replacement: "/new", permanent: true, enabled: true)
  end

  test "viewing redirects index" do
    sign_in(@user)
    visit admin_redirects_path

    assert_text "Redirects"
    assert_text @redirect.regex
    assert_text @redirect.replacement
  end

  test "creating a redirect" do
    sign_in(@user)
    visit new_admin_redirect_path

    fill_in "Regex Pattern", with: "^/legacy$"
    fill_in "Replacement URL", with: "/modern"
    check "Permanent Redirect (301)"
    check "Enabled"
    click_button "Create Redirect"

    assert_text "Redirect was successfully created."
    assert Redirect.exists?(regex: "^/legacy$")
  end

  test "editing a redirect" do
    sign_in(@user)
    visit edit_admin_redirect_path(@redirect)

    fill_in "Replacement URL", with: "/updated"
    click_button "Update Redirect"

    assert_text "Redirect was successfully updated."
    @redirect.reload
    assert_equal "/updated", @redirect.replacement
  end

  test "invalid regex shows error" do
    sign_in(@user)
    visit new_admin_redirect_path

    fill_in "Regex Pattern", with: "["
    fill_in "Replacement URL", with: "/new"
    click_button "Create Redirect"

    assert_text "is not a valid regular expression"
  end

  test "deleting a redirect" do
    skip "This test requires JavaScript support (Selenium)" unless self.class.use_selenium?

    sign_in(@user)
    visit admin_redirects_path

    accept_confirm do
      within("tr", text: @redirect.regex) do
        find("a[title='Delete']").click
      end
    end

    assert_text "Redirect was successfully deleted."
    assert_not Redirect.exists?(id: @redirect.id)
  end
end
