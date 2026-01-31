# frozen_string_literal: true

require "test_helper"
require "ostruct"
require "minitest/mock"

class ImportRssTest < ActiveSupport::TestCase
  test "imports entries, handles images, and records errors" do
    feed_url = "https://feed.example.com/rss"
    image_url = "https://example.com/image.png"

    entry = OpenStruct.new(
      url: "https://example.com/posts/hello-world",
      title: "Hello World",
      published: Time.current,
      content: "<p>Body</p><img src=\"#{image_url}\">",
      summary: "Summary"
    )
    feed = OpenStruct.new(entries: [ entry ])

    image_io_builder = lambda do
      io = StringIO.new("image-data")
      io.define_singleton_method(:content_type) { "image/png" }
      io
    end

    URI.stub(:open, lambda { |url, &block|
      if url == feed_url
        StringIO.new("feed-data")
      else
        io = image_io_builder.call
        block ? block.call(io) : io
      end
    }) do
      Feedjira.stub(:parse, feed) do
        importer = ImportRss.new(feed_url, true)
        assert_difference "Article.count", 1 do
          assert importer.import_data
        end
      end
    end

    URI.stub(:open, lambda { |_url, &block|
      io = StringIO.new("feed-data")
      block ? block.call(io) : io
    }) do
      Feedjira.stub(:parse, ->(_data) { raise StandardError, "boom" }) do
        importer = ImportRss.new(feed_url)
        assert_not importer.import_data
        assert_match(/boom/, importer.error_message)
      end
    end
  end
end
