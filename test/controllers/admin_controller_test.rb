# frozen_string_literal: true

require "test_helper"

class AdminControllerTest < ActionController::TestCase
  def setup
    @user = users(:admin)
    @controller = AdminController.new
    session_record = @user.sessions.create!(user_agent: "Test", ip_address: "127.0.0.1")
    cookies.signed[:session_id] = session_record.id
  end

  test "posts and pages actions assign filtered collections" do
    with_routing do |set|
      set.draw do
        get "admin/posts" => "admin#posts", as: :admin_posts
        get "admin/pages" => "admin#pages", as: :admin_pages
      end

      assert_raises(ActionController::MissingExactTemplate) do
        get :posts, params: { status: "draft" }
      end

      assert_raises(ActionController::MissingExactTemplate) do
        get :pages, params: { status: "all" }
      end
    end
  end
end
