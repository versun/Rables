# frozen_string_literal: true

require "test_helper"

class StaticFileTest < ActiveSupport::TestCase
  test "validates attachment and exposes file metadata helpers" do
    assert_nil StaticFile.new.public_path

    invalid = StaticFile.new(filename: "missing.txt")
    assert_not invalid.valid?
    assert_includes invalid.errors[:file], "must be attached"
    assert_nil invalid.file_size
    assert_nil invalid.file_size_human

    static_file = StaticFile.new(filename: "sample.txt")
    File.open(Rails.root.join("test/fixtures/files/sample.txt")) do |file|
      static_file.file.attach(io: file, filename: "sample.txt", content_type: "text/plain")
    end

    assert static_file.valid?
    assert_equal "/static/sample.txt", static_file.public_path
    assert_equal 28, static_file.file_size
    assert_equal "28 B", static_file.file_size_human
    assert_equal "text/plain", static_file.content_type
  end

  test "file_size_human formats kilobytes and megabytes" do
    kb_file = StaticFile.new(filename: "kb.txt")
    kb_temp = Tempfile.new("kb")
    kb_temp.write("a" * 2048)
    kb_temp.rewind
    kb_file.file.attach(io: kb_temp, filename: "kb.txt", content_type: "text/plain")
    kb_file.save!
    kb_temp.close
    kb_temp.unlink
    assert_match(/KB\z/, kb_file.file_size_human)

    mb_file = StaticFile.new(filename: "mb.txt")
    mb_temp = Tempfile.new("mb")
    mb_temp.write("a" * (2 * 1024 * 1024))
    mb_temp.rewind
    mb_file.file.attach(io: mb_temp, filename: "mb.txt", content_type: "text/plain")
    mb_file.save!
    mb_temp.close
    mb_temp.unlink
    assert_match(/MB\z/, mb_file.file_size_human)
  end
end
