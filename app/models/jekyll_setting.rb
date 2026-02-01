# frozen_string_literal: true

class JekyllSetting < ApplicationRecord
  # Valid git branch name pattern (prevents command injection)
  VALID_BRANCH_PATTERN = /\A[a-zA-Z0-9][a-zA-Z0-9._\/-]*\z/

  # Singleton pattern - only one JekyllSetting record
  def self.instance
    first_or_create
  end

  # Validations
  validates :jekyll_path, presence: true, if: :auto_sync_enabled?
  validates :repository_url, presence: true, if: :git_repository?
  validates :repository_type, inclusion: { in: %w[local git] }
  validates :redirect_export_format, inclusion: { in: %w[netlify vercel htaccess nginx jekyll-plugin] }
  validates :comments_format, inclusion: { in: %w[yaml json] }
  validate :validate_jekyll_path_writable, if: -> { jekyll_path.present? && jekyll_path_changed? }
  validate :validate_front_matter_mapping_json
  validate :validate_branch_name

  # Callbacks
  before_save :normalize_paths

  # Repository type helpers
  def git_repository?
    repository_type == "git"
  end

  def local_repository?
    repository_type == "local"
  end

  # Front matter mapping accessors
  def front_matter_mapping_hash
    return {} if front_matter_mapping.blank?

    JSON.parse(front_matter_mapping)
  rescue JSON::ParserError
    {}
  end

  def front_matter_mapping_hash=(hash)
    self.front_matter_mapping = hash.to_json
  end

  # Path helpers
  def full_posts_path
    return nil if jekyll_path.blank?

    File.join(jekyll_path, posts_directory)
  end

  def full_pages_path
    return nil if jekyll_path.blank?

    File.join(jekyll_path, pages_directory)
  end

  def full_assets_path
    return nil if jekyll_path.blank?

    File.join(jekyll_path, assets_directory)
  end

  def full_images_path
    return nil if jekyll_path.blank?

    File.join(jekyll_path, images_directory)
  end

  def full_static_files_path
    return nil if jekyll_path.blank?

    File.join(jekyll_path, static_files_directory)
  end

  def comments_data_path
    return nil if jekyll_path.blank?

    File.join(jekyll_path, "_data", "comments")
  end

  # Configuration status
  def configured?
    jekyll_path.present? && (!git_repository? || repository_url.present?)
  end

  def ready_for_sync?
    configured? && jekyll_path_valid?
  end

  def jekyll_path_valid?
    return false if jekyll_path.blank?

    File.directory?(jekyll_path) && File.writable?(jekyll_path)
  end

  private

  def normalize_paths
    self.jekyll_path = jekyll_path&.strip&.chomp("/")
    self.repository_url = repository_url&.strip
    self.posts_directory = posts_directory&.strip || "_posts"
    self.pages_directory = pages_directory&.strip || "_pages"
    self.assets_directory = assets_directory&.strip || "assets/images"
    self.images_directory = images_directory&.strip || "assets/images/posts"
    self.static_files_directory = static_files_directory&.strip || "assets"
  end

  def validate_jekyll_path_writable
    return if jekyll_path.blank?

    # Check for path traversal
    if jekyll_path.include?("..") || jekyll_path.include?("~")
      errors.add(:jekyll_path, "contains invalid characters")
      return
    end

    # Check if path exists and is writable
    unless File.directory?(jekyll_path)
      errors.add(:jekyll_path, "does not exist or is not a directory")
      return
    end

    unless File.writable?(jekyll_path)
      errors.add(:jekyll_path, "is not writable")
    end
  end

  def validate_front_matter_mapping_json
    return if front_matter_mapping.blank?

    JSON.parse(front_matter_mapping)
  rescue JSON::ParserError
    errors.add(:front_matter_mapping, "must be valid JSON")
  end

  def validate_branch_name
    return if branch.blank?

    unless branch.match?(VALID_BRANCH_PATTERN)
      errors.add(:branch, "contains invalid characters")
    end

    # Additional security checks
    if branch.include?("..") || branch.start_with?("-")
      errors.add(:branch, "contains invalid characters")
    end
  end
end
