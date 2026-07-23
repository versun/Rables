require "net/http"
require "json"
require "nokogiri"

# Fetches a tweet's author name and text via Twitter's oEmbed endpoint.
# Used to fill an article's Source Reference (author + quote content)
# from the admin UI.
class TwitterOembedService
  MAX_CONTENT_LENGTH = 250

  # Returns { author:, content: } or nil when the fetch fails.
  def fetch(tweet_url)
    data = request_oembed(tweet_url)
    return nil unless data

    doc = Nokogiri::HTML::DocumentFragment.parse(data["html"].to_s)
    content = doc.css("p").map(&:text).join(" ").strip
    content = content[0, MAX_CONTENT_LENGTH] if content.length > MAX_CONTENT_LENGTH

    { author: data["author_name"], content: content }
  rescue => e
    Rails.logger.error "Failed to fetch twitter content: #{e.message}"
    nil
  end

  private

  def request_oembed(tweet_url)
    uri = URI("https://publish.x.com/oembed")
    uri.query = URI.encode_www_form(url: tweet_url, omit_script: true, dnt: true)

    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = true
    http.open_timeout = 5
    http.read_timeout = 5

    response = http.request(Net::HTTP::Get.new(uri))
    response.is_a?(Net::HTTPSuccess) ? JSON.parse(response.body) : nil
  end
end
