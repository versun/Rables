# frozen_string_literal: true

require "x"

# Shared classification for network-level failures that are safe to retry
# via ActiveJob retry_on. Permanent API errors (4xx, validation, auth) are
# still swallowed by the services and logged, matching at-most-once semantics.
module TransientNetworkErrors
  # Server-side API failures (5xx, 429) that are safe to retry, unlike
  # permanent client errors (4xx). Inherits RuntimeError so existing
  # `assert_raises RuntimeError` style handling keeps working.
  class TransientServerError < RuntimeError; end

  TRANSIENT_ERRORS = [
    TransientServerError,
    # x gem: socket-level failures and 5xx responses from the X API.
    # Client errors (4xx, including X::TooManyRequests) stay non-transient.
    X::NetworkError,
    X::ServerError,
    Timeout::Error,
    SocketError,
    SystemCallError,
    EOFError,
    OpenSSL::SSL::SSLError,
    Net::HTTPError
  ].freeze

  private

  def transient_network_error?(error)
    TRANSIENT_ERRORS.any? { |klass| error.is_a?(klass) }
  end
end
