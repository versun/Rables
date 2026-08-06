# frozen_string_literal: true

require "application_system_test_case"

class AdminMigratesTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
  end

  test "export tab shows the full backup form" do
    sign_in(@user)
    visit admin_migrates_path

    assert_text "Full Backup"
    assert_button "Download Backup"
  end

  test "importing from rss" do
    sign_in(@user)
    visit admin_migrates_path(tab: "import")

    fill_in "RSS URL", with: "https://example.com/feed.xml"
    click_button "Import From RSS"

    assert_text "RSS Import in progress"
  end

  test "restoring from a backup without a database fails validation" do
    sign_in(@user)
    visit admin_migrates_path(tab: "import")

    zip_path = create_temp_zip_file
    attach_file "Backup File", zip_path
    click_button "Restore Backup"

    assert_text "Backup archive is missing database.sqlite3"
  ensure
    File.delete(zip_path) if zip_path && File.exist?(zip_path)
  end

  private

  def create_temp_zip_file
    require "zip"
    temp_path = Rails.root.join("tmp", "system_test_backup_#{SecureRandom.hex(4)}.zip")

    Zip::File.open(temp_path, create: true) do |zipfile|
      zipfile.get_output_stream("test.txt") { |f| f.write "test content" }
    end

    temp_path
  end
end
