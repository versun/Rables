require "application_system_test_case"

class SetupTest < ApplicationSystemTestCase
  def setup
    @setting = settings(:default)
  end

  test "setup redirects when already completed" do
    visit setup_path

    assert_current_path new_session_path
    assert_text "Setup has already been completed."
  end

  test "completing setup when incomplete" do
    @setting.update!(setup_completed: false)

    visit setup_path

    fill_in "Username", with: "newadmin"
    fill_in "Password", with: "password123"
    fill_in "Confirm Password", with: "password123"
    fill_in "Site Title", with: "Setup Blog"
    fill_in "Site Description", with: "Setup description"
    fill_in "Author", with: "Setup Author"
    fill_in "Site URL", with: "https://setup.example.com"
    click_button "Complete Setup"

    assert_current_path new_session_path
    assert_text "Setup completed successfully!"
    assert User.exists?(user_name: "newadmin")
  end
end
