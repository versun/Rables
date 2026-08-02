class SocialMediaPost < ApplicationRecord
  belongs_to :article

  validates :platform, presence: true
  validates :url, presence: true
  validates :platform, uniqueness: { scope: :article_id }
end
