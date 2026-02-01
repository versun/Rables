class JekyllStaticFilesExporter
  require "fileutils"

  def initialize(setting: JekyllSetting.instance)
    @setting = setting
    @jekyll_path = Pathname.new(@setting.jekyll_path.to_s)
  end

  def export_all
    StaticFile.find_each { |static_file| export_file(static_file) }
  end

  def export_file(static_file)
    return unless static_file.file.attached?

    target_path = target_path_for(static_file)
    FileUtils.mkdir_p(File.dirname(target_path))
    File.open(target_path, "wb") { |f| f.write(static_file.file.download) }
    target_path
  end

  def build_directory_structure
    FileUtils.mkdir_p(static_files_root)
  end

  def update_references_in_content(content)
    content.to_s.gsub(%r{/static/}, "/#{static_files_directory_prefix}/")
  end

  private

  def static_files_root
    @jekyll_path.join(static_files_directory_prefix)
  end

  def static_files_directory_prefix
    @setting.static_files_directory.to_s.delete_prefix("/")
  end

  def target_path_for(static_file)
    filename = static_file.filename.to_s
    if @setting.preserve_original_paths && filename.include?("/")
      @jekyll_path.join(static_files_directory_prefix, filename)
    else
      @jekyll_path.join(static_files_directory_prefix, File.basename(filename))
    end
  end
end
