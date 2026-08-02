module Exports
  module ZipPackaging
    require "fileutils"
    require "pathname"
    require "zip"

    def create_zip_file
      @zip_path = "#{export_dir}.zip"

      Zip.unicode_names = true
      Zip.force_entry_names_encoding = "UTF-8"

      Zip::OutputStream.open(@zip_path) do |zos|
        Dir.glob(File.join(export_dir.to_s, "**", "*")).sort.each do |file|
          next unless File.file?(file)

          relative_path = Pathname.new(file).relative_path_from(export_dir).to_s
          relative_path = relative_path.tr("\\", "/")
          relative_path = relative_path.encode("UTF-8", invalid: :replace, undef: :replace, replace: "_")

          zos.put_next_entry(relative_path)
          File.open(file, "rb") { |f| IO.copy_stream(f, zos) }
        end
      end

      FileUtils.rm_rf(export_dir)
      @zip_path
    end
  end
end
