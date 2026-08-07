# frozen_string_literal: true

require "pathname"
require "zip"

# Shared helper for packing a directory tree into a single ZIP archive.
# Used by SiteBackup (full backup) and ExportJob (Go migration package).
module ZipArchiver
  Zip.unicode_names = true
  Zip.force_entry_names_encoding = "UTF-8"

  module_function

  def zip_directory(source_dir, zip_path)
    source_dir = Pathname.new(source_dir)
    Zip::OutputStream.open(zip_path.to_s) do |zos|
      Dir.glob(source_dir.join("**", "*")).sort.each do |file|
        next unless File.file?(file)

        relative_path = Pathname.new(file).relative_path_from(source_dir).to_s
        relative_path = relative_path.tr("\\", "/")
        relative_path = relative_path.encode("UTF-8", invalid: :replace, undef: :replace, replace: "_")

        zos.put_next_entry(relative_path)
        File.open(file, "rb") { |f| IO.copy_stream(f, zos) }
      end
    end
  end
end
