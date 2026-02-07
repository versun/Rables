class Redirect < ApplicationRecord
  REGEX_TIMEOUT = 1 # seconds

  validates :regex, presence: true
  validates :replacement, presence: true
  validate :validate_regex_pattern

  after_save :clear_redirect_cache
  after_destroy :clear_redirect_cache

  scope :enabled, -> {
    # Handle both boolean and string/integer values for SQLite compatibility
    where("enabled = ? OR enabled = ? OR enabled = ?", true, 1, "1")
  }

  def match?(path)
    return false unless enabled?
    Timeout.timeout(REGEX_TIMEOUT) { compiled_regex.match?(path) }
  rescue RegexpError, Timeout::Error
    false
  end

  def apply_to(path)
    return nil unless match?(path)
    Timeout.timeout(REGEX_TIMEOUT) do
      result = path.sub(compiled_regex, replacement)
      # If the regex matched the entire path (common case), return the replacement directly
      # Otherwise return the substituted result
      result
    end
  rescue RegexpError, Timeout::Error
    nil
  end

  def enabled?
    # Handle both boolean and string values
    value = read_attribute(:enabled)
    value == true || value == 1 || value.to_s == "1" || value.to_s.downcase == "true"
  end

  def permanent?
    # Handle both boolean and string values
    value = read_attribute(:permanent)
    value == true || value == 1 || value.to_s == "1" || value.to_s.downcase == "true"
  end

  private

  def clear_redirect_cache
    Rails.cache.delete("redirect_middleware/enabled_redirects")
  end

  def compiled_regex
    @compiled_regex ||= Regexp.new(regex)
  end

  def validate_regex_pattern
    return if regex.blank?

    begin
      Regexp.new(regex)
    rescue RegexpError => e
      errors.add(:regex, "is not a valid regular expression: #{e.message}")
    end
  end
end
