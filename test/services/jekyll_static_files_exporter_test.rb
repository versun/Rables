# frozen_string_literal: true

require "test_helper"

class JekyllStaticFilesExporterTest < ActiveSupport::TestCase
  test "exports static file to assets directory" do
    dir = Dir.mktmpdir
    JekyllSetting.create!(jekyll_path: dir, static_files_directory: "assets")

    static_file = StaticFile.new(filename: "sample.txt")
    file = file_fixture("sample.txt")
    static_file.file.attach(io: file.open, filename: "sample.txt", content_type: "text/plain")
    static_file.save!

    exporter = JekyllStaticFilesExporter.new
    exported_path = exporter.export_file(static_file)

    assert exported_path
    assert File.exist?(exported_path)
  ensure
    FileUtils.remove_entry(dir) if dir
  end
end
