class JekyllSetting < ApplicationRecord
  VALID_REPOSITORY_TYPES = %w[local git].freeze
  VALID_COMMENTS_FORMATS = %w[yaml json].freeze
  VALID_REDIRECT_FORMATS = %w[netlify vercel htaccess nginx jekyll-plugin].freeze

  attr_accessor :front_matter_mapping_json

  validates :repository_type, inclusion: { in: VALID_REPOSITORY_TYPES }
  validates :repository_url, presence: true, if: -> { repository_type == "git" }
  validates :comments_format, inclusion: { in: VALID_COMMENTS_FORMATS }, allow_blank: true
  validates :redirect_export_format, inclusion: { in: VALID_REDIRECT_FORMATS }, allow_blank: true
  validate :jekyll_path_must_exist_and_writable, if: -> { jekyll_path.present? }
  validate :repository_url_is_valid, if: -> { repository_type == "git" && repository_url.present? }
  validate :front_matter_mapping_is_valid

  after_initialize :apply_defaults
  before_validation :parse_front_matter_mapping_json

  def self.instance
    first_or_initialize
  end

  def git?
    repository_type == "git"
  end

  def front_matter_mapping_hash
    front_matter_mapping.is_a?(Hash) ? front_matter_mapping : {}
  end

  private

  def apply_defaults
    self.repository_type ||= "local"
    self.branch ||= "main"
    self.posts_directory ||= "_posts"
    self.pages_directory ||= "_pages"
    self.assets_directory ||= "assets/images"
    self.static_files_directory ||= "assets"
    self.comments_format ||= "yaml"
    self.images_directory ||= "assets/images/posts"
    self.auto_sync_enabled = false if auto_sync_enabled.nil?
    self.sync_on_publish = false if sync_on_publish.nil?
    self.preserve_original_paths = false if preserve_original_paths.nil?
    self.export_comments = true if export_comments.nil?
    self.include_pending_comments = false if include_pending_comments.nil?
    self.include_social_comments = true if include_social_comments.nil?
    self.download_remote_images = true if download_remote_images.nil?
  end

  def parse_front_matter_mapping_json
    return if front_matter_mapping_json.blank?

    parsed = JSON.parse(front_matter_mapping_json)
    if parsed.is_a?(Hash)
      self.front_matter_mapping = parsed
    else
      errors.add(:front_matter_mapping_json, "必须是 JSON 对象")
    end
  rescue JSON::ParserError => e
    errors.add(:front_matter_mapping_json, "包含无效的 JSON 格式: #{e.message}")
  end

  def front_matter_mapping_is_valid
    return if front_matter_mapping.blank?

    errors.add(:front_matter_mapping, "必须是 JSON 对象") unless front_matter_mapping.is_a?(Hash)
  end

  def jekyll_path_must_exist_and_writable
    path = jekyll_path.to_s.strip
    return if path.blank?

    pathname = Pathname.new(path)
    unless pathname.absolute?
      errors.add(:jekyll_path, "必须是绝对路径")
      return
    end

    unless pathname.directory?
      errors.add(:jekyll_path, "必须是已存在的目录")
      return
    end

    errors.add(:jekyll_path, "目录不可写") unless File.writable?(pathname)
  end

  def repository_url_is_valid
    url = repository_url.to_s.strip
    return if url.blank?

    return if url.match?(/\A[\w.\-]+@[\w.\-]+:.+\z/)

    uri = URI.parse(url)
    unless uri.scheme.present? && uri.host.present?
      errors.add(:repository_url, "必须是合法的 Git URL")
    end
  rescue URI::InvalidURIError
    errors.add(:repository_url, "必须是合法的 Git URL")
  end
end
