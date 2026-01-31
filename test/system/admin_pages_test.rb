# frozen_string_literal: true

require "application_system_test_case"

class AdminPagesTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @published_page = pages(:published_page)
    @draft_page = pages(:draft_page)
  end

  test "viewing pages index" do
    sign_in(@user)
    visit admin_pages_path

    assert_text "Pages"
    assert_text @published_page.title
    assert_text @draft_page.title
  end

  test "creating a new page" do
    sign_in(@user)
    visit new_admin_page_path

    select "HTML Code", from: "content_type_select"
    fill_in "Title", with: "New Test Page"
    fill_in "page[slug]", with: "new-test-page"
    fill_in "page[html_content]", with: "<p>New page content</p>", visible: :all
    fill_in "Page Order", with: "1"
    select "publish", from: "status_select"
    click_button "Save"

    assert_text "Page was successfully created."
    assert Page.exists?(slug: "new-test-page")
  end

  test "editing a page" do
    sign_in(@user)
    visit edit_admin_page_path(@draft_page)

    fill_in "Title", with: "Updated Page Title"
    click_button "Save"

    assert_text "Page was successfully updated."
    @draft_page.reload
    assert_equal "Updated Page Title", @draft_page.title
  end

  test "publishing a draft page" do
    sign_in(@user)
    visit edit_admin_page_path(@draft_page)

    select "publish", from: "page[status]"
    click_button "Save"

    assert_text "Page was successfully updated."
    @draft_page.reload
    assert @draft_page.publish?
  end

  test "deleting a page" do
    skip "This test requires JavaScript support (Selenium)" unless self.class.use_selenium?

    sign_in(@user)
    visit admin_pages_path

    within("tr", text: @draft_page.title) do
      find("a[title='Trash']").click
    end

    assert_text "Page was successfully deleted."
  end

  test "creating page with validation error shows error" do
    sign_in(@user)
    visit new_admin_page_path

    # Leave title empty (should cause validation error)
    fill_in "page[slug]", with: ""
    click_button "Save"

    # Should stay on form with error
    assert_selector "form"
  end

  test "filtering pages by status" do
    sign_in(@user)
    visit admin_pages_path(status: "publish")

    assert_text @published_page.title
    assert_no_text @draft_page.title
  end
end
