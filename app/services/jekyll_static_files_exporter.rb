class JekyllStaticFilesExporter
  attr_reader :setting

  def initialize(setting = nil)
    @setting = setting || JekyllSetting.instance
  end

  # Export all static files
  def export_all
    return [] unless setting.jekyll_path_valid?

    exported = []
    StaticFile.find_each do |static_file|
      if export_file(static_file)
        exported << static_file
      end
    end
    exported
  end

  # Export single static file
  def export_file(static_file)
    return false unless setting.jekyll_path_valid?
    return false unless static_file.file.attached?

    begin
      target_path = build_target_path(static_file)
      FileUtils.mkdir_p(File.dirname(target_path))

      # Download and save file
      static_file.file.download do |temp_file|
        FileUtils.cp(temp_file.path, target_path)
      end

      log_export(static_file, target_path, :success)
      true
    rescue => e
      log_export(static_file, nil, :failed, e.message)
      false
    end
  end

  # Delete static file from Jekyll directory
  def delete_file(static_file)
    return false unless setting.jekyll_path_valid?

    begin
      target_path = build_target_path(static_file)
      if File.exist?(target_path)
        File.delete(target_path)
        log_export(static_file, target_path, :deleted)
      end
      true
    rescue => e
      log_export(static_file, nil, :failed, e.message)
      false
    end
  end

  # Update content references to use Jekyll paths
  def update_references_in_content(content)
    return content unless content.present?

    doc = Nokogiri::HTML::DocumentFragment.parse(content)

    # Update image sources
    doc.css("img").each do |img|
      src = img["src"]
      next unless src.present? && src.start_with?("/static/")

      jekyll_path = convert_path(src)
      img["src"] = jekyll_path if jekyll_path
    end

    # Update file links
    doc.css("a[href]").each do |link|
      href = link["href"]
      next unless href.present? && href.start_with?("/static/")

      jekyll_path = convert_path(href)
      link["href"] = jekyll_path if jekyll_path
    end

    doc.to_html
  end

  # Build directory structure for static files
  def build_directory_structure
    return unless setting.jekyll_path_valid?

    base_dir = File.join(setting.jekyll_path, setting.static_files_directory)
    FileUtils.mkdir_p(base_dir)

    # Create subdirectories
    FileUtils.mkdir_p(File.join(base_dir, "images"))
    FileUtils.mkdir_p(File.join(base_dir, "documents"))
    FileUtils.mkdir_p(File.join(base_dir, "downloads"))
  end

  private

  def build_target_path(static_file)
    base_dir = File.join(setting.jekyll_path, setting.static_files_directory)

    if setting.preserve_original_paths?
      # Preserve the original path structure
      File.join(base_dir, static_file.filename)
    else
      # Organize by file type
      ext = File.extname(static_file.filename).downcase
      subdir = case ext
      when ".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg"
                 "images"
      when ".pdf", ".doc", ".docx", ".txt"
                 "documents"
      else
                 "downloads"
      end

      File.join(base_dir, subdir, static_file.filename)
    end
  end

  def convert_path(original_path)
    # Convert /static/path/file.ext to /assets/path/file.ext (or configured directory)
    match = original_path.match(%r{^/static/(.+)$})
    return nil unless match

    subpath = match[1]
    "/#{setting.static_files_directory}/#{subpath}"
  end

  def log_export(static_file, target_path, status, message = nil)
    ActivityLog.log!(
      action: :jekyll_static_file_export,
      target: :static_file,
      level: status == :success ? :info : :error,
      filename: static_file.filename,
      target_path: target_path,
      status: status,
      message: message
    )
  end
end
