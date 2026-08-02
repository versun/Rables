# frozen_string_literal: true

require "test_helper"
require "minitest/mock"
require "vips"

class ActiveStorageCompressionTest < ActiveSupport::TestCase
  test "compresses only embed image attachments" do
    article = articles(:published_article)
    rich_text = ActionText::RichText.create!(record: article, name: "content", body: "<p>Hi</p>")

    image_blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("fake-image"),
      filename: "image.png",
      content_type: "image/png"
    )
    text_blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("fake-text"),
      filename: "file.txt",
      content_type: "text/plain"
    )

    image_attachment = ActiveStorage::Attachment.create!(name: "embeds", record: rich_text, blob: image_blob)
    non_embed_attachment = ActiveStorage::Attachment.create!(name: "other", record: rich_text, blob: image_blob)
    non_image_attachment = ActiveStorage::Attachment.create!(name: "embeds", record: rich_text, blob: text_blob)

    image_attachment.stub(:compress_image, true) do
      assert image_attachment.send(:image_attachment?)
      image_attachment.send(:compress_trix_image)
    end

    non_embed_attachment.stub(:compress_image, -> { flunk("should not compress non-embeds") }) do
      non_embed_attachment.send(:compress_trix_image)
    end

    non_image_attachment.stub(:compress_image, -> { flunk("should not compress non-image") }) do
      non_image_attachment.send(:compress_trix_image)
    end

    fake_vips = Class.new do
      def get_typeof(_field) = 0

      def autorot = self

      # Enough of the vips API for ActiveStorage's image analyzer, which also
      # goes through the stubbed Vips::Image.new_from_file during blob.analyze
      def get(_field) = ""

      def width = 1

      def height = 1

      def write_to_file(path, **_kwargs)
        File.write(path, "tiny")
      end
    end.new

    Vips::Image.stub(:new_from_file, fake_vips) do
      image_attachment.send(:compress_image)
    end

    original_path = image_attachment.blob.service.path_for(image_attachment.blob.key)
    assert_equal File.size(original_path), image_attachment.blob.reload.byte_size
    assert_equal Digest::MD5.file(original_path).base64digest, image_attachment.blob.checksum
  end

  test "refreshes checksum and byte_size so blob.open passes integrity verification" do
    original_size = nil
    attachment = attach_real_image(filename: "noisy.jpg", content_type: "image/jpeg") do |path|
      # High-entropy image saved at high quality re-encodes much smaller at Q80
      Vips::Image.gaussnoise(400, 300, mean: 128, sigma: 60).cast("uchar").write_to_file(path, Q: 95)
      original_size = File.size(path)
    end

    blob = attachment.blob.reload
    path = blob.service.path_for(blob.key)

    assert_operator blob.byte_size, :<, original_size
    assert_equal File.size(path), blob.byte_size
    assert_equal Digest::MD5.file(path).base64digest, blob.checksum
    assert_nothing_raised { blob.open { |io| io.read } }
    assert blob.metadata["analyzed"], "metadata should be re-analyzed after compression"
  end

  test "does not replace the original when the compressed copy is not smaller" do
    attachment = attach_real_image(filename: "flat.jpg", content_type: "image/jpeg") do |path|
      # Already heavily compressed image re-encodes larger at Q80
      Vips::Image.black(64, 64).cast("uchar").write_to_file(path, Q: 10)
    end

    blob = attachment.blob
    original_checksum = blob.checksum
    original_size = blob.byte_size

    attachment.send(:compress_image)

    blob.reload
    assert_equal original_size, blob.byte_size
    assert_equal original_checksum, blob.checksum
  end

  test "skips GIF images" do
    attachment = attach_real_image(filename: "animated.gif", content_type: "image/gif") do |path|
      Vips::Image.black(50, 50).cast("uchar").write_to_file(path)
    end

    blob = attachment.blob
    original_checksum = blob.checksum
    original_size = blob.byte_size

    attachment.send(:compress_image)

    blob.reload
    assert_equal original_size, blob.byte_size
    assert_equal original_checksum, blob.checksum
  end

  private

  # Creates an "embeds" attachment backed by a real image generated with vips.
  # The block writes the image content to the given tempfile path before upload.
  def attach_real_image(filename:, content_type:)
    article = articles(:published_article)
    rich_text = ActionText::RichText.create!(record: article, name: "content", body: "<p>Hi</p>")

    Tempfile.create([ File.basename(filename, ".*"), File.extname(filename) ]) do |file|
      file.binmode
      yield file.path
      file.flush

      blob = ActiveStorage::Blob.create_and_upload!(
        io: File.open(file.path, "rb"),
        filename: filename,
        content_type: content_type
      )
      return ActiveStorage::Attachment.create!(name: "embeds", record: rich_text, blob: blob)
    end
  end
end
