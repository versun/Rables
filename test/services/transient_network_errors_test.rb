# frozen_string_literal: true

require "test_helper"

class TransientNetworkErrorsTest < ActiveSupport::TestCase
  class Dummy
    include TransientNetworkErrors
  end

  test "classifies x gem network and server errors as transient" do
    dummy = Dummy.new

    network_error = X::NetworkError.new("connection failed")
    server_error = X::ServerError.new(response: build_response("500", "Internal Server Error"))

    assert dummy.send(:transient_network_error?, network_error)
    assert dummy.send(:transient_network_error?, server_error)
  end

  test "does not classify x gem client errors as transient" do
    dummy = Dummy.new

    too_many_requests = X::TooManyRequests.new(response: build_response("429", "Too Many Requests"))
    unauthorized = X::Unauthorized.new(response: build_response("401", "Unauthorized"))

    refute dummy.send(:transient_network_error?, too_many_requests)
    refute dummy.send(:transient_network_error?, unauthorized)
  end

  private

  def build_response(code, message)
    response = Net::HTTPResponse::CODE_TO_OBJ[code].new("1.1", code, message)
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "{}")
    response
  end
end
