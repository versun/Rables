# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

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

    unless defined?(Vips)
      Object.const_set(:Vips, Module.new)
      Vips.const_set(:Image, Class.new)
    end

    fake_vips = Class.new do
      def write_to_file(path, **_kwargs)
        File.write(path, "compressed")
      end
    end.new

    Vips::Image.stub(:new_from_file, fake_vips) do
      image_attachment.send(:compress_image)
    end

    original_path = image_attachment.blob.service.path_for(image_attachment.blob.key)
    assert_equal File.size(original_path), image_attachment.blob.reload.byte_size
  end
end
