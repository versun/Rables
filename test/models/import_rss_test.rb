# frozen_string_literal: true

require "test_helper"
require "ostruct"
require "minitest/mock"

class ImportRssTest < ActiveSupport::TestCase
  PUBLIC_IPS = [ "93.184.216.34" ].freeze

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

    Resolv.stub(:getaddresses, PUBLIC_IPS) do
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
          assert_equal 1, importer.imported_count
          assert_equal 0, importer.failed_count
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

    article = Article.find_by(slug: "hello-world")
    refute_match(/<!DOCTYPE|<html|<body/i, article.content.to_s)
  ensure
    article&.destroy
  end

  test "rejects feed urls resolving to private addresses" do
    importer = ImportRss.new("http://169.254.169.254/latest/meta-data")

    Resolv.stub(:getaddresses, [ "169.254.169.254" ]) do
      assert_not importer.import_data
    end

    assert_match(/unsafe feed url/i, importer.error_message)
  end

  test "rejects feed urls when dns resolution fails" do
    importer = ImportRss.new("https://unresolvable.example.com/rss")

    Resolv.stub(:getaddresses, []) do
      assert_not importer.import_data
    end

    assert_match(/unsafe feed url/i, importer.error_message)
  end

  test "counts failed entries without aborting the import" do
    feed_url = "https://feed.example.com/rss"
    Article.create!(title: "Existing", slug: "bad", status: :publish, content: "<p>existing</p>")

    bad_entry = OpenStruct.new(
      url: "https://example.com/posts/bad",
      title: "Bad",
      published: Time.current,
      content: "<p>Bad</p>",
      summary: "Bad"
    )
    good_entry = OpenStruct.new(
      url: "https://example.com/posts/good",
      title: "Good",
      published: Time.current,
      content: "<p>Good</p>",
      summary: "Good"
    )
    feed = OpenStruct.new(entries: [ bad_entry, good_entry ])

    Resolv.stub(:getaddresses, PUBLIC_IPS) do
      URI.stub(:open, ->(_url, &block) { block ? block.call(StringIO.new("feed-data")) : StringIO.new("feed-data") }) do
        Feedjira.stub(:parse, feed) do
          importer = ImportRss.new(feed_url)
          assert_difference "Article.count", 1 do
            assert importer.import_data
          end
          assert_equal 1, importer.imported_count
          assert_equal 1, importer.failed_count
        end
      end
    end

    completed_log = ActivityLog.where(target: "import", action: "completed").last
    assert_includes completed_log.description, "failed_count=1"
  ensure
    Article.find_by(slug: "bad")&.destroy
    Article.find_by(slug: "good")&.destroy
  end

  test "skips images that fail to download instead of aborting" do
    feed_url = "https://feed.example.com/rss"
    entry = OpenStruct.new(
      url: "https://example.com/posts/broken-image",
      title: "Broken Image",
      published: Time.current,
      content: "<p>Body</p><img src=\"https://example.com/broken.png\">",
      summary: "Summary"
    )
    feed = OpenStruct.new(entries: [ entry ])

    Resolv.stub(:getaddresses, PUBLIC_IPS) do
      URI.stub(:open, lambda { |url, &block|
        raise "boom" if url.include?("broken.png")
        io = StringIO.new("feed-data")
        block ? block.call(io) : io
      }) do
        Feedjira.stub(:parse, feed) do
          importer = ImportRss.new(feed_url, true)
          assert_difference "Article.count", 1 do
            assert importer.import_data
          end
        end
      end
    end

    article = Article.find_by!(slug: "broken-image")
    assert_includes article.content.to_s, "broken.png"
  ensure
    article&.destroy
  end

  test "skips images that resolve to private addresses" do
    feed_url = "https://feed.example.com/rss"
    entry = OpenStruct.new(
      url: "https://example.com/posts/internal-image",
      title: "Internal Image",
      published: Time.current,
      content: "<p>Body</p><img src=\"http://192.168.1.1/internal.png\">",
      summary: "Summary"
    )
    feed = OpenStruct.new(entries: [ entry ])

    resolver = ->(host) { host.include?("192.168") ? [ "192.168.1.1" ] : PUBLIC_IPS }
    Resolv.stub(:getaddresses, resolver) do
      URI.stub(:open, lambda { |url, &block|
        raise "must not fetch internal url" if url.include?("192.168")
        io = StringIO.new("feed-data")
        block ? block.call(io) : io
      }) do
        Feedjira.stub(:parse, feed) do
          importer = ImportRss.new(feed_url, true)
          assert_no_difference "ActiveStorage::Blob.count" do
            assert_difference "Article.count", 1 do
              assert importer.import_data
            end
          end
        end
      end
    end

    article = Article.find_by!(slug: "internal-image")
    assert_includes article.content.to_s, "192.168.1.1/internal.png"
  ensure
    article&.destroy
  end
end
