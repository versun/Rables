class TwitterArchiveConnection < ApplicationRecord
  RELATIONSHIP_TYPES = %w[follower following].freeze

  validates :account_id, presence: true
  validates :relationship_type, presence: true, inclusion: { in: RELATIONSHIP_TYPES }

  scope :followers, -> { where(relationship_type: "follower") }
  scope :following, -> { where(relationship_type: "following") }
  scope :unresolved_screen_name, -> { where(screen_name: [ nil, "" ]) }

  def display_label
    screen_name.present? ? "@#{screen_name}" : "Account ID: #{account_id}"
  end
end
