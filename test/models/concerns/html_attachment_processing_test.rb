# frozen_string_literal: true

require "test_helper"
require "minitest/mock"
require "ostruct"

class HtmlAttachmentProcessingTest < ActiveSupport::TestCase
  class Harness
    include Exports::HtmlAttachmentProcessing
    attr_reader :attachments_dir

    def initialize(attachments_dir)
      @attachments_dir = attachments_dir
    end
  end

  test "builds full urls and resolves filenames/extensions" do
    Dir.mktmpdir do |dir|
      processor = Harness.new(dir)

      Setting.stub(:table_exists?, true) do
        Setting.stub(:first, OpenStruct.new(url: "http://example.com")) do
          assert_equal "http://example.com/relative.png", processor.send(:build_full_url, "relative.png")
          assert_equal "http://example.com/absolute.png", processor.send(:build_full_url, "/absolute.png")
        end
      end

      processor.stub(:detect_extension_from_content_type, ".png") do
        filename = processor.send(:extract_filename_from_url, "http://example.com/noext")
        assert_match(/\.png\z/, filename)
      end

      invalid_filename = processor.send(:extract_filename_from_url, "http://\n")
      assert_match(/\.jpg\z/, invalid_filename)

      head_response = Net::HTTPOK.new("1.1", "200", "OK")
      head_response["Content-Type"] = "image/webp"
      head_response.instance_variable_set(:@read, true)
      http = Struct.new(:response) { def head(_url) response end }.new(head_response)
      Net::HTTP.stub(:start, ->(*_args, **_kwargs, &block) { block.call(http) }) do
        assert_equal ".webp", processor.send(:detect_extension_from_content_type, "http://example.com/img.webp")
      end
    end
  end

  test "downloads active storage attachments to the export directory" do
    Dir.mktmpdir do |dir|
      processor = Harness.new(dir)
      blob = ActiveStorage::Blob.create_and_upload!(
        io: StringIO.new("blob"),
        filename: "blob.png",
        content_type: "image/png"
      )
      blob_url = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)

      result = processor.send(:download_and_save_attachment, blob_url, "blob.png", 1, "article")
      assert_equal true, result.start_with?("attachments/article_1/")
      local_path = File.join(dir, "article_1", File.basename(result))
      assert File.exist?(local_path)
    end
  end
end
