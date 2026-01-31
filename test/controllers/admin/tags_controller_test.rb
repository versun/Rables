# frozen_string_literal: true

require "test_helper"

class Admin::TagsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    @tag = tags(:ruby)
    sign_in(@user)
  end

  test "admin tags CRUD and batch destroy" do
    get admin_tags_path
    assert_response :success

    get new_admin_tag_path
    assert_response :success

    assert_difference "Tag.count", 1 do
      post admin_tags_path, params: { tag: { name: "New Tag" } }
    end
    assert_redirected_to admin_tags_path

    post admin_tags_path, params: { tag: { name: "" } }
    assert_response :success

    get edit_admin_tag_path(@tag)
    assert_response :success

    patch admin_tag_path(@tag), params: { tag: { name: "Updated Tag" } }
    assert_redirected_to admin_tags_path
    assert_equal "Updated Tag", @tag.reload.name

    patch admin_tag_path(@tag), params: { tag: { name: "" } }
    assert_response :success

    article = articles(:published_article)
    article.tags << @tag unless article.tags.include?(@tag)

    assert_difference "Tag.count", -1 do
      delete admin_tag_path(@tag)
    end
    assert_redirected_to admin_tags_path
    assert_not_includes article.reload.tags, @tag

    batch_tag = Tag.create!(name: "Batch Tag")

    assert_difference "Tag.count", -1 do
      post batch_destroy_admin_tags_path, params: { ids: [ batch_tag.id ] }
    end
    assert_redirected_to admin_tags_path
  end
end
