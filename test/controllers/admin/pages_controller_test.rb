# frozen_string_literal: true

require "test_helper"

class Admin::PagesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    @page = pages(:draft_page)
    sign_in(@user)
  end

  test "admin pages CRUD and batch actions" do
    get admin_pages_path
    assert_response :success

    get new_admin_page_path
    assert_response :success

    get edit_admin_page_path(@page.slug)
    assert_response :success

    assert_difference "Page.count", 1 do
      post admin_pages_path, params: {
        page: {
          title: "New Page",
          slug: "new-page",
          status: "draft",
          content_type: "html",
          html_content: "<p>Content</p>"
        }
      }
    end
    assert_redirected_to admin_pages_path

    post admin_pages_path, params: {
      page: {
        title: "",
        slug: "",
        status: "draft",
        content_type: "html",
        html_content: "<p>Content</p>"
      }
    }
    assert_response :unprocessable_entity

    patch admin_page_path(@page.slug), params: { page: { title: "Updated Page" } }
    assert_redirected_to admin_pages_path
    assert_equal "Updated Page", @page.reload.title

    scheduled_time = Time.zone.parse("2026-09-01 10:00")
    patch admin_page_path(@page.slug), params: { page: { status: "schedule", scheduled_at: scheduled_time } }
    assert_redirected_to admin_pages_path
    assert_equal scheduled_time, @page.reload.scheduled_at

    patch admin_page_path(@page.slug), params: { page: { title: "" } }
    assert_response :unprocessable_entity

    post batch_publish_admin_pages_path, params: { ids: [ @page.slug ] }
    assert_redirected_to admin_pages_path
    assert @page.reload.publish?

    post batch_unpublish_admin_pages_path, params: { ids: [ @page.slug ] }
    assert_redirected_to admin_pages_path
    assert @page.reload.draft?

    delete_page = Page.create!(
      title: "Delete Page",
      slug: "delete-page",
      status: :draft,
      content_type: :html,
      html_content: "<p>Content</p>"
    )

    assert_difference "Page.count", -1 do
      post batch_destroy_admin_pages_path, params: { ids: [ delete_page.slug ] }
    end

    assert_difference "Page.count", -1 do
      delete admin_page_path(@page.slug)
    end
  end

  test "new page form checks enable comments by default" do
    get new_admin_page_path
    assert_response :success
    assert_select "input#page_comment[checked]"
  end

  test "create validation failure keeps comment checkbox unchecked" do
    post admin_pages_path, params: {
      page: {
        title: "",
        slug: "",
        status: "draft",
        content_type: "html",
        html_content: "<p>Content</p>",
        comment: "0"
      }
    }
    assert_response :unprocessable_entity
    assert_select "input#page_comment[checked]", 0
  end
end
