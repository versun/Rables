class Comment < ApplicationRecord
  belongs_to :commentable, polymorphic: true
  # Keep belongs_to :article for backward compatibility
  belongs_to :article, optional: true
  belongs_to :parent, class_name: "Comment", optional: true
  has_many :replies, class_name: "Comment", foreign_key: "parent_id", dependent: :destroy

  # Validations for all comments
  validates :author_name, presence: true
  validates :content, presence: true
  validates :commentable_id, presence: true
  validates :commentable_type, presence: true

  # Validations for external comments
  validates :platform, presence: true, if: :external_comment?
  validates :external_id, presence: true, if: :external_comment?
  validates :external_id, uniqueness: { scope: [ :commentable_type, :commentable_id, :platform ] }, if: :external_comment?

  # Optional URL validation for native comments
  validates :author_url, format: { with: URI::DEFAULT_PARSER.make_regexp(%w[http https]), message: "must be a valid URL" }, allow_blank: true
  validates :url, format: { with: URI::DEFAULT_PARSER.make_regexp(%w[http https]), message: "must be a valid URL" }, allow_blank: true
  validates :author_email, format: { with: URI::MailTo::EMAIL_REGEXP, message: "must be a valid email" }, allow_blank: true

  # Validate that parent comment belongs to the same commentable
  validate :parent_belongs_to_same_commentable, if: :parent_id?
  validate :parent_is_not_self, if: :parent_id?

  # Scopes
  enum :status, { pending: 0, approved: 1, rejected: 2 }, default: :pending

  scope :local, -> { where(platform: nil) }
  scope :mastodon, -> { where(platform: "mastodon") }
  scope :bluesky, -> { where(platform: "bluesky") }
  scope :twitter, -> { where(platform: "twitter") }
  scope :top_level, -> { where(parent_id: nil) }

  default_scope { order(published_at: :asc) }

  after_commit :enqueue_reply_notification, on: [ :create, :update ]

  # Creates or updates an external platform comment for the given commentable.
  # Returns [ comment, result ] where result is :created, :updated or :unchanged.
  def self.upsert_from_external(commentable, platform, comment_data, status: nil)
    comment = commentable.comments.find_or_initialize_by(
      platform: platform,
      external_id: comment_data[:external_id]
    )

    attributes = {
      author_name: comment_data[:author_name],
      author_username: comment_data[:author_username],
      author_avatar_url: comment_data[:author_avatar_url],
      content: comment_data[:content],
      published_at: comment_data[:published_at],
      url: comment_data[:url]
    }
    # Only apply the requested status to new comments so re-fetching does not
    # overwrite a moderation decision already made in the admin.
    attributes[:status] = status if status && comment.new_record?
    comment.assign_attributes(attributes)

    result = :unchanged
    if comment.new_record?
      comment.save!
      result = :created
    elsif comment.changed?
      comment.save!
      result = :updated
    end

    [ comment, result ]
  end

  def display_commentable
    commentable || parent&.commentable || article
  end

  private

  def external_comment?
    platform.present? || external_id.present?
  end

  def parent_belongs_to_same_commentable
    return unless parent_id?

    # Check if parent exists
    parent_record = Comment.find_by(id: parent_id)
    unless parent_record
      errors.add(:parent_id, "does not exist")
      return
    end

    # Check if parent belongs to the same commentable
    if parent_record.commentable_type != commentable_type || parent_record.commentable_id != commentable_id
      errors.add(:parent_id, "must belong to the same #{commentable_type}")
    end
  end

  def parent_is_not_self
    errors.add(:parent_id, "cannot reference itself") if parent_id == id
  end

  def enqueue_reply_notification
    return unless saved_change_to_status?
    return unless approved?
    return unless parent_id?
    return unless platform.nil?

    parent_comment = parent
    return unless parent_comment&.author_email.present?
    return unless parent_comment.platform.nil?

    if author_email.present? && author_email.casecmp?(parent_comment.author_email.to_s)
      return
    end

    CommentReplyNotificationJob.perform_later(id)
  end
end
