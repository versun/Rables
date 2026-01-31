# frozen_string_literal: true

require "application_system_test_case"

class AdminMigratesTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
  end

  test "exporting default format" do
    sign_in(@user)
    visit admin_migrates_path

    click_button "Export Default Format"
    assert_text "Export Initiated"
  end

  test "exporting markdown format" do
    sign_in(@user)
    visit admin_migrates_path

    click_button "Export Markdown Format"
    assert_text "Markdown Export Initiated"
  end

  test "importing from rss" do
    sign_in(@user)
    visit admin_migrates_path(tab: "import")

    fill_in "RSS URL", with: "https://example.com/feed.xml"
    click_button "Import From RSS"

    assert_text "RSS Import in progress"
  end

  test "importing from zip" do
    sign_in(@user)
    visit admin_migrates_path(tab: "import")

    zip_path = create_temp_zip_file
    attach_file "ZIP File", zip_path
    click_button "Import From ZIP"

    assert_text "ZIP Import in progress"
  ensure
    File.delete(zip_path) if zip_path && File.exist?(zip_path)
  end

  private

  def create_temp_zip_file
    require "zip"
    temp_path = Rails.root.join("tmp", "system_test_import_#{SecureRandom.hex(4)}.zip")

    Zip::File.open(temp_path, create: true) do |zipfile|
      zipfile.get_output_stream("test.txt") { |f| f.write "test content" }
    end

    temp_path
  end
end
