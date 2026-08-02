class Page < ApplicationRecord
  has_rich_text :content
  has_many :comments, as: :commentable, dependent: :destroy
  enum :status, [ :draft, :publish, :schedule, :trash, :shared ]
  enum :content_type, { rich_text: "rich_text", html: "html" }, default: "rich_text"

  validates :title, presence: true
  validates :slug, presence: true, uniqueness: true
  validates :redirect_url, url: true, allow_blank: true
  validates :html_content, presence: true, if: -> { html? }
  validates :scheduled_at, presence: true, if: :schedule?
  validate :rich_text_content_presence

  scope :published, -> { where(status: :publish) }
  scope :scheduled, -> { where(status: :schedule) }
  scope :by_status, ->(status) { where(status: status) }
  scope :publishable, -> { where(status: :schedule).where("scheduled_at <= ?", Time.current) }

  after_save :schedule_publication, if: :should_schedule?

  def to_param
    slug
  end

  def redirect?
    redirect_url.present?
  end

  def should_publish?
    schedule? && scheduled_at.present? && scheduled_at <= Time.current
  end

  def should_schedule?
    schedule?
  end

  def publish_scheduled
    with_lock do
      reload
      return unless should_publish?

      update(status: :publish, scheduled_at: nil)
    end

    # The navbar lists published pages; bust its cache so the page shows up
    CacheableSettings.refresh_navbar_items if publish?
  end

  def schedule_publication
    PublishScheduledPagesJob.schedule_at(self)
  end

  # 根据content_type返回相应的内容
  def rendered_content
    if html?
      html_content
    else
      content
    end
  end

  private

  def rich_text_content_presence
    if rich_text?
      text = content.present? ? content.to_plain_text.to_s.strip : ""
      if text.blank?
        errors.add(:content, "can't be blank")
      end
    end
  end
end
