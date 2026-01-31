# frozen_string_literal: true

require "application_system_test_case"

class AdminTagsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @ruby_tag = tags(:ruby)
    @rails_tag = tags(:rails)
  end

  test "viewing tags index" do
    sign_in(@user)
    visit admin_tags_path

    assert_text "Tags"
    assert_text @ruby_tag.name
    assert_text @rails_tag.name
  end

  test "creating a new tag" do
    sign_in(@user)
    visit new_admin_tag_path

    fill_in "Name", with: "New Test Tag"
    click_button "Save"

    assert_text "Tag was successfully created."
    assert Tag.exists?(name: "New Test Tag")
  end

  test "editing a tag" do
    sign_in(@user)
    visit edit_admin_tag_path(@ruby_tag)

    fill_in "Name", with: "Ruby Programming"
    click_button "Save"

    assert_text "Tag was successfully updated."
    @ruby_tag.reload
    assert_equal "Ruby Programming", @ruby_tag.name
  end

  test "deleting a tag" do
    skip "This test requires JavaScript support (Selenium)" unless self.class.use_selenium?

    sign_in(@user)
    visit admin_tags_path

    accept_confirm do
      within("tr", text: @rails_tag.name) do
        find("a[title='Delete']").click
      end
    end

    assert_text "Tag was successfully deleted."
    assert_not Tag.exists?(id: @rails_tag.id)
  end

  test "creating tag with duplicate name shows error" do
    sign_in(@user)
    visit new_admin_tag_path

    fill_in "Name", with: @ruby_tag.name
    click_button "Save"

    assert_selector "form"
  end

  test "tags are listed alphabetically" do
    sign_in(@user)
    visit admin_tags_path

    tag_names = all("tbody tr td.col-title a").map(&:text)
    assert_equal tag_names, tag_names.sort
  end
end
