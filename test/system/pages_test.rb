# frozen_string_literal: true

require "application_system_test_case"

class PagesTest < ApplicationSystemTestCase
  def setup
    @published_page = pages(:published_page)
    @draft_page = pages(:draft_page)
    @shared_page = pages(:shared_page)
    @user = users(:admin)
  end

  test "viewing a published page" do
    visit page_path(@published_page.slug)

    assert_text "Published page content"
    assert_selector "textarea[placeholder='Comment *']"
    assert_button "Submit"
  end

  test "viewing a shared page" do
    visit page_path(@shared_page.slug)

    assert_text "Shared page content"
  end

  test "draft page returns 404 for unauthenticated" do
    visit page_path(@draft_page.slug)

    assert_text "The page you were looking for doesn't exist."
  end

  test "draft page is visible when authenticated" do
    sign_in(@user)
    visit page_path(@draft_page.slug)

    assert_text "Draft page content"
  end
end
