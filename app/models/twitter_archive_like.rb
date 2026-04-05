class TwitterArchiveLike < ApplicationRecord
  validates :tweet_id, presence: true, uniqueness: true
end
