# frozen_string_literal: true

require "test_helper"

class ApplicationCable::ConnectionTest < ActionCable::Connection::TestCase
  test "connects with valid session" do
    user = create_user(user_name: "cable-user", password: "password123")
    session = Session.create!(user: user)

    cookies.signed[:session_id] = session.id
    connect

    assert_equal user, connection.current_user
  end

  test "rejects when session is missing" do
    assert_reject_connection { connect }
  end
end
