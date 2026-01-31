# frozen_string_literal: true

require "application_system_test_case"

class MobileSidebarTest < ApplicationSystemTestCase
  setup do
    skip "Selenium driver required for sidebar interaction" unless self.class.use_selenium?
    page.driver.browser.manage.window.resize_to(390, 844)
  end

  test "mobile sidebar toggles from menu button" do
    visit root_path

    assert_selector ".mobile-menu-btn", visible: true
    assert_no_selector ".sidebar.sidebar-open"

    find(".mobile-menu-btn").click
    assert_selector ".sidebar.sidebar-open"
    assert_selector ".sidebar-overlay.overlay-visible"

    find(".sidebar-overlay").click
    assert_no_selector ".sidebar.sidebar-open"
  end
end
