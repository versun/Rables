# frozen_string_literal: true

require "test_helper"

class JekyllStaticFilesExporterTest < ActiveSupport::TestCase
  setup do
    @setting = JekyllSetting.instance
    @temp_dir = Dir.mktmpdir("jekyll_test")
    @setting.update!(
      jekyll_path: @temp_dir,
      repository_type: "local",
      static_files_directory: "assets",
      preserve_original_paths: false,
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )
    @exporter = JekyllStaticFilesExporter.new(@setting)
  end

  teardown do
    FileUtils.rm_rf(@temp_dir) if @temp_dir && File.exist?(@temp_dir)
  end

  test "initializes with default setting" do
    exporter = JekyllStaticFilesExporter.new
    assert_equal JekyllSetting.instance, exporter.setting
  end

  test "initializes with custom setting" do
    exporter = JekyllStaticFilesExporter.new(@setting)
    assert_equal @setting, exporter.setting
  end

  test "initializes stats" do
    assert_equal 0, @exporter.stats[:exported]
    assert_equal 0, @exporter.stats[:errors]
  end

  test "export_all returns nil when path invalid" do
    @setting.jekyll_path = "/nonexistent"
    result = @exporter.export_all
    assert_nil result
  end

  test "export_all returns stats when successful" do
    result = @exporter.export_all
    assert_kind_of Hash, result
    assert result.key?(:exported)
    assert result.key?(:errors)
  end

  test "export_file returns nil when path invalid" do
    @setting.jekyll_path = "/nonexistent"
    # Create a static file with attachment
    static_file = StaticFile.new(filename: "test.txt")
    static_file.file.attach(
      io: StringIO.new("content"),
      filename: "test.txt",
      content_type: "text/plain"
    )
    static_file.save!

    result = @exporter.export_file(static_file)
    assert_nil result
  end

  test "export_file returns nil when file not attached" do
    # Build but don't save - can't create without attachment
    static_file = StaticFile.new(filename: "test-#{Time.current.to_i}.txt")
    result = @exporter.export_file(static_file)
    assert_nil result
  end

  test "export_file with attached file exports to target directory" do
    static_file = StaticFile.new(filename: "test-export-#{Time.current.to_i}.txt")
    static_file.file.attach(
      io: StringIO.new("test content"),
      filename: "test.txt",
      content_type: "text/plain"
    )
    static_file.save!

    result = @exporter.export_file(static_file)

    assert result
    assert_equal 1, @exporter.stats[:exported]

    expected_path = File.join(@temp_dir, "assets", "test.txt")
    assert File.exist?(expected_path)
    assert_equal "test content", File.read(expected_path)
  end

  test "export_file creates parent directories" do
    static_file = StaticFile.new(filename: "nested-file-#{Time.current.to_i}.txt")
    static_file.file.attach(
      io: StringIO.new("content"),
      filename: "file.txt",
      content_type: "text/plain"
    )
    static_file.save!

    @exporter.export_file(static_file)

    expected_path = File.join(@temp_dir, "assets", "file.txt")
    assert File.exist?(expected_path)
  end

  test "export_all processes all static files" do
    2.times do |i|
      static_file = StaticFile.new(filename: "file#{i}-#{Time.current.to_i}.txt")
      static_file.file.attach(
        io: StringIO.new("content #{i}"),
        filename: "file#{i}.txt",
        content_type: "text/plain"
      )
      static_file.save!
    end

    @exporter.export_all

    assert_equal 2, @exporter.stats[:exported]
  end

  # Security tests
  test "sanitize_path removes path traversal attempts" do
    exporter = JekyllStaticFilesExporter.new(@setting)

    # Test via private method access
    assert_equal "foo/bar.txt", exporter.send(:sanitize_path, "../foo/bar.txt")
    assert_equal "foo/bar.txt", exporter.send(:sanitize_path, "../../foo/bar.txt")
    assert_equal "foo/bar.txt", exporter.send(:sanitize_path, "foo/../bar.txt")
    assert_equal "foo/bar.txt", exporter.send(:sanitize_path, "/foo/bar.txt")
    assert_equal "foo/bar.txt", exporter.send(:sanitize_path, "//foo//bar.txt")
  end

  test "safe_path blocks paths outside jekyll directory" do
    exporter = JekyllStaticFilesExporter.new(@setting)

    # Safe paths
    assert exporter.send(:safe_path?, File.join(@temp_dir, "assets/file.txt"))
    assert exporter.send(:safe_path?, File.join(@temp_dir, "nested/deep/file.txt"))

    # Unsafe paths
    refute exporter.send(:safe_path?, "/etc/passwd")
    refute exporter.send(:safe_path?, File.join(@temp_dir, "../outside.txt"))
    refute exporter.send(:safe_path?, "/tmp/other_dir/file.txt")
  end

  test "export_file blocks path traversal in preserve_original_paths mode" do
    @setting.update!(preserve_original_paths: true)

    # Create a static file with a malicious filename containing path traversal
    malicious_filename = "../../../tmp/jekyll_traversal_test_#{SecureRandom.hex(8)}.txt"
    static_file = StaticFile.new(filename: malicious_filename)
    static_file.file.attach(
      io: StringIO.new("malicious content"),
      filename: "test.txt",
      content_type: "text/plain"
    )
    static_file.save!

    result = @exporter.export_file(static_file)

    # The file should be exported but path traversal should be sanitized
    # The file should end up inside the Jekyll directory, not at /tmp/
    assert result
    assert_equal 1, @exporter.stats[:exported]

    # Verify the file was NOT written outside the Jekyll directory
    # The sanitized path removes .. so it becomes tmp/jekyll_traversal_test_xxx.txt
    sanitized_name = malicious_filename.gsub(/\.\./, "").gsub(/\A\/+/, "")
    outside_path = "/#{sanitized_name}"
    refute File.exist?(outside_path), "File should NOT be written outside Jekyll directory"

    # Verify the file was written inside the Jekyll directory (sanitized path)
    expected_safe_path = File.join(@temp_dir, sanitized_name)
    assert File.exist?(expected_safe_path), "File should be written to sanitized path inside Jekyll dir"
    assert_equal "malicious content", File.read(expected_safe_path)
  end
end
