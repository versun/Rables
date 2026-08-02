class Tag < ApplicationRecord
  has_many :article_tags, dependent: :destroy
  has_many :articles, through: :article_tags
  has_many :subscriber_tags, dependent: :destroy
  has_many :subscribers, through: :subscriber_tags

  validates :name, presence: true, uniqueness: { case_sensitive: false }
  validates :slug, presence: true, uniqueness: true

  before_validation :generate_slug

  after_commit :refresh_has_tags_cache, on: %i[create destroy]
  # Renaming a tag changes what article list fragments render; bust their caches
  after_update_commit :touch_articles, if: :saved_change_to_name?

  scope :alphabetical, -> { order(:name) }

  # Find or create tags by comma-separated names
  def self.find_or_create_by_names(names_string)
    return [] if names_string.blank?

    names_string.split(",").map(&:strip).reject(&:blank?).uniq.map do |name|
      # Case-insensitive search to match validation behavior
      begin
        where("LOWER(name) = ?", name.downcase).first || create!(name: name)
      rescue ActiveRecord::RecordNotUnique
        # Lost an insert race; the other process created the tag
        where("LOWER(name) = ?", name.downcase).first!
      end
    end
  end

  # Count of articles using this tag
  def articles_count
    articles.count
  end

  private

  def refresh_has_tags_cache
    CacheableSettings.refresh_has_tags
  end

  def touch_articles
    articles.touch_all
  end

  def generate_slug
    return if slug.present? || name.blank?

    # 直接使用名称作为slug，支持中文
    # 只做基本的清理：去除首尾空格，将多个空格替换为单个空格
    slug_candidate = name.to_s.strip.gsub(/\s+/, " ")

    # 确保slug唯一
    base_slug = slug_candidate
    counter = 1
    while Tag.where(slug: slug_candidate).where.not(id: id || 0).exists?
      slug_candidate = "#{base_slug}-#{counter}"
      counter += 1
    end

    self.slug = slug_candidate
  end
end
