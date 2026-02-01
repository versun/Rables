# frozen_string_literal: true

require "application_system_test_case"

class AdminJekyllTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @setting = JekyllSetting.instance
    @temp_dir = Dir.mktmpdir("jekyll_test")
  end

  def teardown
    FileUtils.rm_rf(@temp_dir) if @temp_dir && File.exist?(@temp_dir)
  end

  test "viewing jekyll settings page" do
    sign_in(@user)
    visit admin_jekyll_path

    assert_text "Jekyll Integration"
    assert_text "Configuration"
    assert_text "Basic Settings"
    assert_text "Directory Settings"
    assert_text "Sync Settings"
    assert_text "Export Settings"
  end

  test "updating jekyll settings" do
    sign_in(@user)
    visit admin_jekyll_path

    fill_in "Jekyll Project Path", with: @temp_dir
    select "Local Only", from: "Repository Type"
    fill_in "Posts Directory", with: "_posts"
    fill_in "Pages Directory", with: "_pages"
    fill_in "Assets Directory", with: "assets/images"
    fill_in "Images Directory", with: "assets/images/posts"

    click_button "Save Settings"

    assert_text "Jekyll settings updated successfully"
    @setting.reload
    assert_equal @temp_dir, @setting.jekyll_path
    assert_equal "local", @setting.repository_type
  end

  test "updating jekyll settings with git repository" do
    skip "Requires JavaScript for dynamic form fields" unless self.class.use_selenium?

    sign_in(@user)
    visit admin_jekyll_path

    fill_in "Jekyll Project Path", with: @temp_dir
    select "Git Repository", from: "Repository Type"

    # Wait for JavaScript to show the git settings
    assert_selector "#git-settings", visible: true

    fill_in "Repository URL", with: "https://github.com/test/repo.git"
    fill_in "Branch", with: "main"

    click_button "Save Settings"

    assert_text "Jekyll settings updated successfully"
    @setting.reload
    assert_equal "git", @setting.repository_type
    assert_equal "https://github.com/test/repo.git", @setting.repository_url
    assert_equal "main", @setting.branch
  end

  test "invalid jekyll path shows error" do
    sign_in(@user)
    visit admin_jekyll_path

    fill_in "Jekyll Project Path", with: "/nonexistent/path"
    click_button "Save Settings"

    assert_text "does not exist or is not a directory"
  end

  test "viewing sync status when not configured" do
    sign_in(@user)
    visit admin_jekyll_path

    assert_text "Not Configured"
  end

  test "viewing sync status when configured" do
    @setting.update!(
      jekyll_path: @temp_dir,
      repository_type: "local",
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )

    sign_in(@user)
    visit admin_jekyll_path

    assert_text "Configured"
    assert_text "Never synced"
  end

  test "viewing sync status after sync" do
    @setting.update!(
      jekyll_path: @temp_dir,
      repository_type: "local",
      last_sync_at: 1.hour.ago,
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )

    sign_in(@user)
    visit admin_jekyll_path

    assert_text "Configured"
    assert_text "ago"
  end

  test "viewing sync history link" do
    sign_in(@user)
    visit admin_jekyll_path

    assert_link "View Sync History"
  end

  test "export settings options" do
    sign_in(@user)
    visit admin_jekyll_path

    assert_select "Redirect Export Format"
    assert_select "Comments Export Format"
    assert_text "Export Comments"
    assert_text "Include Social Media Comments"
    assert_text "Download Remote Images"
  end

  test "front matter mapping field" do
    sign_in(@user)
    visit admin_jekyll_path

    assert_text "Front Matter Mapping (JSON)"
    assert_selector "textarea[name='jekyll_setting[front_matter_mapping]']"
  end

  test "updating export settings" do
    sign_in(@user)
    visit admin_jekyll_path

    fill_in "Jekyll Project Path", with: @temp_dir
    select "Vercel (vercel.json)", from: "Redirect Export Format"
    select "JSON", from: "Comments Export Format"
    check "Export Comments"
    check "Include Social Media Comments"
    check "Download Remote Images"

    click_button "Save Settings"

    assert_text "Jekyll settings updated successfully"
    @setting.reload
    assert_equal "vercel", @setting.redirect_export_format
    assert_equal "json", @setting.comments_format
    assert @setting.export_comments
    assert @setting.include_social_comments
    assert @setting.download_remote_images
  end
end
