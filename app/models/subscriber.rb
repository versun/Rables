class Subscriber < ApplicationRecord
  has_many :subscriber_tags, dependent: :destroy
  has_many :tags, through: :subscriber_tags

  validates :email, presence: true, uniqueness: true, format: { with: URI::MailTo::EMAIL_REGEXP }
  before_create :generate_tokens

  scope :confirmed, -> { where.not(confirmed_at: nil) }
  scope :active, -> { confirmed.where(unsubscribed_at: nil) }

  # 查找订阅了特定tag的订阅者
  scope :subscribed_to_tag, ->(tag) { joins(:tags).where(tags: { id: tag.id }) }

  # 查找订阅了任何指定tags的订阅者（用于文章发送）
  scope :subscribed_to_any_tags, ->(tag_ids) {
    return all if tag_ids.blank?
    joins(:tags).where(tags: { id: tag_ids }).distinct
  }

  # 查找没有订阅任何tag的订阅者（订阅所有内容）
  scope :subscribed_to_all, -> { left_joins(:tags).where(tags: { id: nil }) }

  def confirmed?
    confirmed_at.present?
  end

  def active?
    confirmed? && unsubscribed_at.nil?
  end

  def confirm!
    update(confirmed_at: Time.current) unless confirmed?
  end

  def unsubscribe!
    update(unsubscribed_at: Time.current) unless unsubscribed?
  end

  def unsubscribed?
    unsubscribed_at.present?
  end

  # 检查是否订阅了特定tag
  def subscribed_to_tag?(tag)
    tags.include?(tag)
  end

  # 检查是否订阅了任何内容（有订阅tag或订阅所有）
  def has_subscriptions?
    tags.any?
  end

  # 检查是否订阅所有内容（没有指定tag）
  def subscribed_to_all?
    tags.empty?
  end

  private

  # Keep pre-set tokens (e.g. from zip imports) so old confirm/unsubscribe links stay valid
  def generate_tokens
    self.confirmation_token ||= SecureRandom.urlsafe_base64(32)
    self.unsubscribe_token ||= SecureRandom.urlsafe_base64(32)
  end
end
