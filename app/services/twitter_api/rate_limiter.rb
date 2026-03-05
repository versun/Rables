module TwitterApi
  # Handles rate limiting logic for Twitter/X API
  class RateLimiter
    # Twitter API v2 limits:
    # - GET /2/tweets/:id: 300 requests per 15 minutes (20 RPM)
    # - GET /2/tweets/search/recent: 180 requests per 15 minutes (12 RPM)
    # We use conservative 10 RPM (6 seconds between requests)
    DELAY_BETWEEN_REQUESTS = 6 # seconds
    DEFAULT_MAX_RETRIES = 3
    BASE_BACKOFF_TIME = 15 # seconds

    def initialize
      @last_request_time = nil
    end

    # Make a rate-limited API request with delay between requests
    def make_request(client, endpoint, max_retries: DEFAULT_MAX_RETRIES)
      retries = 0

      loop do
        begin
          wait_for_rate_limit

          response = client.get(endpoint)
          @last_request_time = Time.current

          check_for_rate_limit_error(response)

          return response
        rescue => e
          raise unless rate_limit_error?(e)

          retries += 1
          if retries <= max_retries
            wait_time = calculate_backoff_time(retries)
            log_retry(wait_time, retries, max_retries)
            sleep(wait_time)
            # continue loop
          else
            log_max_retries_exceeded(max_retries)
            log_rate_limit_exceeded(build_exceeded_rate_limit_info)
            raise
          end
        end
      end
    end

    # Make a rate-limited request and return rate limit info
    def make_request_with_info(client, endpoint, max_retries: DEFAULT_MAX_RETRIES)
      retries = 0

      loop do
        begin
          wait_for_rate_limit

          response = client.get(endpoint)
          @last_request_time = Time.current

          check_for_rate_limit_error(response)

          rate_limit_info = build_default_rate_limit_info

          return [ response, rate_limit_info ]
        rescue => e
          raise unless rate_limit_error?(e)

          retries += 1
          if retries <= max_retries
            wait_time = calculate_backoff_time(retries)
            log_retry(wait_time, retries, max_retries)
            log_rate_limit_exceeded(build_rate_limit_info_with_wait(wait_time))
            sleep(wait_time)
            # continue loop
          else
            log_max_retries_exceeded(max_retries)
            log_rate_limit_exceeded(build_exceeded_rate_limit_info)
            return [ nil, build_exceeded_rate_limit_info ]
          end
        end
      end
    end

    # Execute a block with retry logic for rate limits
    def with_retry(max_retries: DEFAULT_MAX_RETRIES)
      retries = 0

      begin
        yield
      rescue => e
        raise unless rate_limit_error?(e)

        retries += 1
        wait_time = retry_wait_time(e, retries)
        rate_limit_info = rate_limit_info_from_error(e, wait_time)

        if retries <= max_retries
          log_retry(wait_time, retries, max_retries)
          log_rate_limit_exceeded(rate_limit_info)
          sleep(wait_time)
          retry
        end

        log_max_retries_exceeded(max_retries)
        log_rate_limit_exceeded(rate_limit_info)
        raise
      end
    end

    # Check if an error is a rate limit error
    def rate_limit_error?(error)
      x_rate_limit_error = defined?(X::TooManyRequests) && error.is_a?(X::TooManyRequests)
      x_rate_limit_error || rate_limit_error_message?(error.message.to_s)
    end

    # Calculate exponential backoff time for retries
    def calculate_backoff_time(retry_count)
      # Exponential backoff: 15s, 30s, 60s
      [ BASE_BACKOFF_TIME * (2 ** (retry_count - 1)), 300 ].min # Cap at 5 minutes
    end

    # Extract rate limit info from error
    def rate_limit_info_from_error(error, wait_time)
      if defined?(X::TooManyRequests) && error.is_a?(X::TooManyRequests)
        rate_limit = error.rate_limit
        return {
          limit: rate_limit&.limit,
          remaining: rate_limit&.remaining,
          reset_at: rate_limit&.reset_at || Time.current + wait_time
        }
      end

      {
        limit: 180,
        remaining: 0,
        reset_at: Time.current + wait_time
      }
    end

    private

    def wait_for_rate_limit
      return unless @last_request_time

      elapsed = Time.current - @last_request_time
      if elapsed < DELAY_BETWEEN_REQUESTS
        sleep(DELAY_BETWEEN_REQUESTS - elapsed)
      end
    end

    def check_for_rate_limit_error(response)
      return unless response.is_a?(Hash) && response["errors"]

      error = response["errors"].first
      if error && (error["title"]&.include?("Too Many Requests") || error["detail"]&.include?("Too Many Requests"))
        raise "Rate limit exceeded: #{error["detail"] || error["title"]}"
      end
    end

    def rate_limit_error_message?(message)
      message.include?("Too Many Requests") || message.include?("Rate limit") || message.include?("429")
    end

    def retry_wait_time(error, retry_count)
      backoff_time = calculate_backoff_time(retry_count)
      return backoff_time unless defined?(X::TooManyRequests) && error.is_a?(X::TooManyRequests)

      [ error.retry_after.to_i, backoff_time ].max
    end

    def build_default_rate_limit_info
      {
        limit: 180,
        remaining: nil,
        reset_at: Time.current + 15.minutes
      }
    end

    def build_exceeded_rate_limit_info
      {
        limit: 180,
        remaining: 0,
        reset_at: Time.current + 15.minutes
      }
    end

    def build_rate_limit_info_with_wait(wait_time)
      {
        limit: 180,
        remaining: 0,
        reset_at: Time.current + wait_time
      }
    end

    def log_retry(wait_time, retry_count, max_retries)
      Rails.event.notify "twitter_service.rate_limit_retry",
        level: "warn",
        component: "Twitter::RateLimiter",
        wait_time: wait_time,
        retry_count: retry_count,
        max_retries: max_retries
    end

    def log_max_retries_exceeded(max_retries)
      Rails.event.notify "twitter_service.rate_limit_exceeded",
        level: "error",
        component: "Twitter::RateLimiter",
        max_retries: max_retries
    end

    def log_rate_limit_exceeded(rate_limit_info)
      reset_time = rate_limit_info[:reset_at] || Time.current + 15.minutes
      wait_seconds = [ (reset_time - Time.current).to_i, 0 ].max

      Rails.event.notify "twitter_service.rate_limit_exceeded_event",
        level: "error",
        component: "Twitter::RateLimiter",
        reset_time: reset_time,
        wait_seconds: wait_seconds

      ActivityLog.log!(
        action: :rate_limited,
        target: :twitter_api,
        level: :error,
        reset_at: reset_time,
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit]
      )
    end
  end
end
