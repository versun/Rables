# frozen_string_literal: true

require "test_helper"

class ActiveStorage::Blobs::BlobTest < ActionView::TestCase
  test "renders download link for non-previewable attachments" do
    file = file_fixture("sample.txt")
    blob = ActiveStorage::Blob.create_and_upload!(
      io: file.open,
      filename: "sample.txt",
      content_type: "text/plain"
    )

    with_active_storage_url_options(host: "example.com") do
      render partial: "active_storage/blobs/blob", locals: { blob: blob }

      assert_select "a[href=?]", rails_blob_url(blob, disposition: "attachment"),
                    text: blob.filename.to_s
    end
  end

  private

  def with_active_storage_url_options(options)
    previous = ActiveStorage::Current.url_options
    ActiveStorage::Current.url_options = options
    yield
  ensure
    ActiveStorage::Current.url_options = previous
  end
end
