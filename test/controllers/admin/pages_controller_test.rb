# frozen_string_literal: true

require "test_helper"

class Admin::PagesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    @page = pages(:draft_page)
    sign_in(@user)
  end

  test "admin pages CRUD reorder and batch actions" do
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
    assert_response :success

    patch admin_page_path(@page.slug), params: { page: { title: "Updated Page" } }
    assert_redirected_to admin_pages_path
    assert_equal "Updated Page", @page.reload.title

    patch admin_page_path(@page.slug), params: { page: { title: "" } }
    assert_response :success

    with_page_insert_at(true) do
      patch reorder_admin_page_path(@page.slug), params: { position: 1 }
      assert_response :ok
    end

    with_page_insert_at(false) do
      patch reorder_admin_page_path(@page.slug), params: { position: 2 }
      assert_response :unprocessable_entity
    end

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

  private

  def with_page_insert_at(result)
    Page.class_eval do
      define_method(:insert_at) { |_position| result }
    end
    yield
  ensure
    Page.class_eval do
      remove_method(:insert_at) if method_defined?(:insert_at)
    end
  end
end
