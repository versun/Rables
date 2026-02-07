module HttpRedirectHandler
  extend ActiveSupport::Concern

  # Shared HTTP configuration constants
  MAX_REDIRECTS = 5
  DEFAULT_OPEN_TIMEOUT = 10
  DEFAULT_READ_TIMEOUT = 10
  DEFAULT_WRITE_TIMEOUT = 10

  # Follow HTTP redirects to fetch content
  # @param uri [URI] The URI to fetch
  # @param limit [Integer] Maximum number of redirects to follow
  # @return [Net::HTTPResponse] The final response
  def fetch_with_redirect(uri, limit = MAX_REDIRECTS)
    raise "Too many HTTP redirects" if limit == 0

    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = (uri.scheme == "https")
    http.open_timeout = DEFAULT_OPEN_TIMEOUT
    http.read_timeout = DEFAULT_READ_TIMEOUT

    path = uri.path.presence || "/"
    path += "?#{uri.query}" if uri.query
    request = Net::HTTP::Get.new(path)
    response = http.request(request)

    case response
    when Net::HTTPSuccess
      response
    when Net::HTTPRedirection
      redirect_uri = URI.parse(response["location"])
      if redirect_uri.relative?
        redirect_uri = URI.join("#{uri.scheme}://#{uri.host}:#{uri.port}", response["location"])
      end
      log_redirect(redirect_uri) if respond_to?(:log_redirect, true)
      fetch_with_redirect(redirect_uri, limit - 1)
    else
      response
    end
  end

  # Download a remote image with redirect support
  # @param image_url [String] The URL of the image
  # @return [Array<String, String>, nil] [image_data, content_type] or nil on failure
  def download_remote_image_with_redirect(image_url)
    return nil unless image_url.present?

    # Convert relative URL to absolute
    if image_url.start_with?("/")
      site_url = Setting.first&.url.presence || "http://localhost:3000"
      image_url = "#{site_url}#{image_url}"
    end

    uri = URI.parse(image_url)
    response = fetch_with_redirect(uri)

    return nil unless response.is_a?(Net::HTTPSuccess)

    [response.body, response["content-type"] || "image/jpeg"]
  rescue StandardError => e
    log_download_error(e, image_url) if respond_to?(:log_download_error, true)
    nil
  end
end
