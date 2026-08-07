# frozen_string_literal: true

require "test_helper"
require "minitest/mock"
require "zip"

class ExportJobTest < ActiveJob::TestCase
  setup do
    @exports_dir = Rails.root.join("tmp", "exports")
    FileUtils.mkdir_p(@exports_dir)
    @created = []
  end

  teardown do
    @created.each { |path| FileUtils.rm_rf(path) }
  end

  test "backup export completes and records the artifact" do
    export = Export.create!(kind: "backup")
    zip_path = make_zip("backup_test.zip")

    SiteBackup.stub(:export, Pathname.new(zip_path)) do
      ExportJob.perform_now(export.id)
    end

    export.reload
    assert export.completed?
    assert_equal "backup_test.zip", export.filename
    assert_equal File.size(zip_path), export.byte_size
    assert ActivityLog.where(target: "export", action: "completed").exists?
  end

  test "site_export zips the package and removes the work directory" do
    export = Export.create!(kind: "site_export")

    fake_export = ->(output_dir:, **) do
      FileUtils.mkdir_p(Pathname.new(output_dir).join("data"))
      File.write(Pathname.new(output_dir).join("data", "articles.jsonl"), "{}\n")
      Pathname.new(output_dir)
    end

    SiteExport.stub(:export, fake_export) do
      ExportJob.perform_now(export.id)
    end

    export.reload
    assert export.completed?
    assert_match(/\Asite_export_.*\.zip\z/, export.filename)

    zip_path = export.file_path
    @created << zip_path
    assert_equal [ "data/articles.jsonl" ], Zip::File.open(zip_path.to_s) { |zip| zip.entries.map(&:name) }
    assert_not Dir.exist?(zip_path.to_s.delete_suffix(".zip"))
  end

  test "records the failure on the export instead of raising" do
    export = Export.create!(kind: "backup")

    SiteBackup.stub(:export, ->(**) { raise "disk exploded" }) do
      ExportJob.perform_now(export.id)
    end

    export.reload
    assert export.failed?
    assert_equal "disk exploded", export.error
    assert ActivityLog.where(target: "export", action: "failed").exists?
  end

  private

  def make_zip(name)
    path = @exports_dir.join(name)
    Zip::File.open(path, create: true) do |zipfile|
      zipfile.get_output_stream("test.txt") { |f| f.write "test" }
    end
    @created << path
    path
  end
end
