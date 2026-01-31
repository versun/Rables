# frozen_string_literal: true

require "test_helper"

class UserTest < ActiveSupport::TestCase
  def setup
    @user = User.new(
      user_name: "testuser",
      password: "password123",
      password_confirmation: "password123"
    )
  end

  test "should be valid with valid attributes" do
    assert @user.valid?
  end

  test "should require user_name" do
    @user.user_name = nil
    assert_not @user.valid?
    assert_includes @user.errors[:user_name], "can't be blank"
  end

  test "should require unique user_name" do
    existing_user = users(:admin)
    @user.user_name = existing_user.user_name
    assert_not @user.valid?
    assert_includes @user.errors[:user_name], "has already been taken"
  end

  test "should normalize user_name to lowercase" do
    @user.user_name = "TestUser"
    @user.save!
    assert_equal "testuser", @user.user_name
  end

  test "should strip whitespace from user_name" do
    @user.user_name = "  testuser  "
    @user.save!
    assert_equal "testuser", @user.user_name
  end

  test "should require password" do
    @user.password = nil
    assert_not @user.valid?
  end

  test "should authenticate with correct password" do
    @user.save!
    assert @user.authenticate("password123")
  end

  test "should not authenticate with incorrect password" do
    @user.save!
    assert_not @user.authenticate("wrongpassword")
  end

  test "should have many sessions" do
    @user.save!
    assert_respond_to @user, :sessions
  end

  test "should destroy associated sessions when destroyed" do
    @user.save!
    @user.sessions.create!(
      ip_address: "127.0.0.1",
      user_agent: "Test Agent"
    )

    assert_difference "Session.count", -1 do
      @user.destroy
    end
  end
end
