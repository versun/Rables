class TwitterArchiveTweet < ApplicationRecord
  has_many_attached :media

  ENTRY_TYPES = %w[tweet reply retweet_quote].freeze

  validates :tweet_id, presence: true, uniqueness: true
  validates :entry_type, presence: true, inclusion: { in: ENTRY_TYPES }
  validates :screen_name, presence: true
  validates :tweeted_at, presence: true

  scope :chronological_desc, -> { order(tweeted_at: :desc, tweet_id: :desc) }
  scope :tweets, -> { where(entry_type: "tweet") }
  scope :replies, -> { where(entry_type: "reply") }
  scope :retweet_quotes, -> { where(entry_type: "retweet_quote") }

  def self.tab_for(value)
    normalized = value.to_s
    ENTRY_TYPES.include?(normalized) ? normalized : "tweet"
  end

  def self.for_tab(value)
    where(entry_type: tab_for(value)).chronological_desc
  end

  def tweet_url
    return nil if screen_name.blank? || screen_name == "archive"

    "https://twitter.com/#{screen_name}/status/#{tweet_id}"
  end
end
