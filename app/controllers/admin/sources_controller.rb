require "json"

class Admin::SourcesController < Admin::BaseController
  # POST /admin/sources/fetch_twitter
  # Fetch tweet content for source reference
  def fetch_twitter
    url = params[:url]

    if url.blank?
      render json: { error: "URL is required" }, status: :unprocessable_entity
      return
    end

    unless twitter_url?(url)
      render json: { error: "Not a valid Twitter/X URL" }, status: :unprocessable_entity
      return
    end

    result = TwitterOembedService.new.fetch(url)

    if result
      render json: {
        success: true,
        author: result[:author],
        content: result[:content]
      }
    else
      render json: { error: "Failed to fetch tweet content" }, status: :service_unavailable
    end
  end

  private

  def twitter_url?(url)
    uri = URI.parse(url)
    host = uri.host.to_s.downcase
    %w[twitter.com www.twitter.com x.com www.x.com].include?(host)
  rescue URI::InvalidURIError
    false
  end
end
