# frozen_string_literal: true

require "application_system_test_case"

class Admin::JekyllTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @temp_dir = Dir.mktmpdir
  end

  def teardown
    FileUtils.remove_entry(@temp_dir) if @temp_dir
  end

  test "configures jekyll settings and triggers sync" do
    sign_in(@user)
    visit admin_jekyll_path

    fill_in "Jekyll Path", with: @temp_dir
    click_button "Save"

    assert_text "Jekyll settings updated."

    click_button "Sync Now"
    assert_text "Jekyll sync has started."
  end
end
