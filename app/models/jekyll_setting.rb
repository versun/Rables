class JekyllSetting < ApplicationRecord
  REPOSITORY_TYPES = %w[local git].freeze
  REDIRECT_FORMATS = %w[netlify vercel htaccess nginx jekyll-plugin].freeze
  COMMENTS_FORMATS = %w[yaml json].freeze

  validates :repository_type, inclusion: { in: REPOSITORY_TYPES }
  validates :redirect_export_format, inclusion: { in: REDIRECT_FORMATS }
  validates :comments_format, inclusion: { in: COMMENTS_FORMATS }
  validates :jekyll_path, presence: true, if: :auto_sync_enabled?
  validate :jekyll_path_must_be_writable, if: -> { jekyll_path.present? }
  validate :repository_url_must_be_valid, if: -> { repository_type == "git" && repository_url.present? }
  validate :front_matter_mapping_must_be_valid_json

  # Singleton pattern - always use first record
  def self.instance
    first_or_create!
  end

  def self.update_instance(attributes)
    instance.update(attributes)
  end

  # Check if Jekyll path is configured and valid
  def jekyll_path_valid?
    return false if jekyll_path.blank?
    return false unless Dir.exist?(jekyll_path)
    return false unless File.writable?(jekyll_path)

    true
  end

  # Get full path for posts directory
  def posts_full_path
    return nil unless jekyll_path_valid?

    File.join(jekyll_path, posts_directory)
  end

  # Get full path for pages directory
  def pages_full_path
    return nil unless jekyll_path_valid?

    File.join(jekyll_path, pages_directory)
  end

  # Get full path for assets directory
  def assets_full_path
    return nil unless jekyll_path_valid?

    File.join(jekyll_path, assets_directory)
  end

  # Get full path for images directory
  def images_full_path
    return nil unless jekyll_path_valid?

    File.join(jekyll_path, images_directory)
  end

  # Get parsed front matter mapping
  def front_matter_mapping_hash
    return {} if front_matter_mapping.blank?

    JSON.parse(front_matter_mapping)
  rescue JSON::ParserError
    {}
  end

  # Check if Git integration is enabled
  def git_enabled?
    repository_type == "git" && repository_url.present?
  end

  private

  def jekyll_path_must_be_writable
    return unless Dir.exist?(jekyll_path)
    return if File.writable?(jekyll_path)

    errors.add(:jekyll_path, "must be writable")
  end

  def repository_url_must_be_valid
    return if repository_url.match?(%r{^(https?://|git@)})

    errors.add(:repository_url, "must be a valid Git URL")
  end

  def front_matter_mapping_must_be_valid_json
    return if front_matter_mapping.blank?

    JSON.parse(front_matter_mapping)
  rescue JSON::ParserError
    errors.add(:front_matter_mapping, "must be valid JSON")
  end
end
