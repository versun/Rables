require "uri"

class TwitterArchivesController < ApplicationController
  allow_unauthenticated_access only: :show
  helper_method :safe_archive_url

  PER_PAGE = 20

  TABS = {
    "tweet" => "Tweets",
    "reply" => "Replies",
    "retweet_quote" => "Retweets / Quotes",
    "follower" => "Followers",
    "following" => "Following",
    "like" => "Likes"
  }.freeze

  def show
    @tabs = TABS
    @active_tab = archive_tab(params[:tab])
    @last_archive_upload_at = TwitterArchiveImport.last_imported_at

    case @active_tab
    when *TwitterArchiveTweet::ENTRY_TYPES
      @archive_tweets = paginate_archive_scope(TwitterArchiveTweet.with_attached_media.for_tab(@active_tab))
      @archive_collection = @archive_tweets
    when "follower"
      @archive_connections = paginate_archive_scope(TwitterArchiveConnection.followers.order(:user_link, :account_id))
      @archive_collection = @archive_connections
    when "following"
      @archive_connections = paginate_archive_scope(TwitterArchiveConnection.following.order(:user_link, :account_id))
      @archive_collection = @archive_connections
    when "like"
      @archive_likes = paginate_archive_scope(TwitterArchiveLike.order(created_at: :desc, tweet_id: :desc))
      @archive_collection = @archive_likes
    end
  end

  private

  def paginate_archive_scope(scope)
    scope.paginate(page: params[:page], per_page: PER_PAGE)
  end

  def archive_tab(value)
    normalized = value.to_s
    TABS.key?(normalized) ? normalized : "tweet"
  end

  def safe_archive_url(value)
    uri = URI.parse(value.to_s.strip)
    uri.to_s if uri.is_a?(URI::HTTP) && uri.host.present?
  rescue URI::InvalidURIError
    nil
  end
end
