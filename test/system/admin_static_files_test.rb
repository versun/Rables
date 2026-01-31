# frozen_string_literal: true

require "application_system_test_case"

class AdminStaticFilesTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    file_path = Rails.root.join("test/fixtures/files/sample.txt")
    @static_file = StaticFile.new(filename: "sample.txt", description: "Sample file")
    @static_file.file.attach(io: File.open(file_path), filename: "sample.txt", content_type: "text/plain")
    @static_file.save!
  end

  test "viewing static files index" do
    sign_in(@user)
    visit admin_static_files_path

    assert_text "Static Files Management"
    assert_text @static_file.filename
    assert_text @static_file.description
  end

  test "uploading a new static file" do
    sign_in(@user)
    visit admin_static_files_path

    file_path = Rails.root.join("test/fixtures/files/sample.txt")
    attach_file "Choose File", file_path
    fill_in "Description (Optional)", with: "Uploaded from system test"
    click_button "Upload File"

    assert_text "文件上传成功（已覆盖同名文件）"
    assert StaticFile.exists?(filename: "sample.txt")
  end

  test "uploading without file shows error" do
    sign_in(@user)
    visit admin_static_files_path

    click_button "Upload File"
    assert_text "请选择要上传的文件"
  end

  test "deleting a static file" do
    skip "This test requires JavaScript support (Selenium)" unless self.class.use_selenium?

    sign_in(@user)
    visit admin_static_files_path

    accept_confirm do
      within("tr", text: @static_file.filename) do
        click_button "Delete"
      end
    end

    assert_text "文件 #{@static_file.filename} 已删除"
    assert_not StaticFile.exists?(id: @static_file.id)
  end
end
