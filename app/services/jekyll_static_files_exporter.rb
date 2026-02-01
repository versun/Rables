# frozen_string_literal: true

class JekyllStaticFilesExporter
  attr_reader :setting, :stats

  def initialize(setting = nil)
    @setting = setting || JekyllSetting.instance
    @stats = { exported: 0, errors: 0 }
  end

  def export_all
    return unless @setting.jekyll_path_valid?

    StaticFile.find_each do |static_file|
      export_file(static_file)
    end

    @stats
  end

  def export_file(static_file)
    return unless @setting.jekyll_path_valid?
    return unless static_file.file.attached?

    begin
      target_path = build_target_path(static_file)

      # Security: Verify target path is within Jekyll directory
      unless safe_path?(target_path)
        Rails.event.notify("jekyll_static_files_exporter.path_traversal_blocked",
          component: "JekyllStaticFilesExporter",
          file_path: static_file.path,
          target_path: target_path,
          level: "error")
        @stats[:errors] += 1
        return false
      end

      FileUtils.mkdir_p(File.dirname(target_path))

      # Download from ActiveStorage
      static_file.file.blob.download do |chunk|
        File.open(target_path, "ab") { |f| f.write(chunk) }
      end

      @stats[:exported] += 1
      true
    rescue => e
      @stats[:errors] += 1
      Rails.event.notify("jekyll_static_files_exporter.export_failed",
        component: "JekyllStaticFilesExporter",
        file_path: static_file.filename,
        error: e.message,
        level: "error")
      false
    end
  end

  private

  def build_target_path(static_file)
    if @setting.preserve_original_paths?
      # Sanitize path to prevent traversal
      sanitized_path = sanitize_path(static_file.filename)
      File.join(@setting.jekyll_path, sanitized_path)
    else
      # Place all files in static_files_directory
      filename = sanitize_filename(static_file.file.filename.to_s)
      File.join(@setting.full_static_files_path, filename)
    end
  end

  def sanitize_path(path)
    # Remove path traversal attempts
    path.to_s
        .gsub(/\.\./, "")           # Remove ..
        .gsub(%r{//+}, "/")         # Collapse multiple slashes
        .gsub(/\A\/+/, "")          # Remove leading slashes
        .gsub(/[^\w.\-\/]/, "_")    # Replace invalid chars
  end

  def sanitize_filename(filename)
    filename.to_s
            .gsub(/\.\./, "")
            .gsub(%r{[/\\]}, "")
            .gsub(/[^\w.\-]/, "_")
  end

  def safe_path?(target_path)
    # Expand both paths to resolve any remaining symlinks or relative components
    expanded_target = File.expand_path(target_path)
    expanded_jekyll = File.expand_path(@setting.jekyll_path)

    # Ensure target is within Jekyll directory
    expanded_target.start_with?(expanded_jekyll + "/") || expanded_target == expanded_jekyll
  end
end
