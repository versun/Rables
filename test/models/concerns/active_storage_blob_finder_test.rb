# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class ActiveStorageBlobFinderTest < ActiveSupport::TestCase
  setup do
    @blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("fake-image"),
      filename: "image.png",
      content_type: "image/png"
    )
  end

  test "finds blob from redirect and proxy urls" do
    redirect_url = "/rails/active_storage/blobs/redirect/#{@blob.signed_id}/image.png"
    proxy_url = "/rails/active_storage/blobs/proxy/#{@blob.signed_id}/image.png"

    assert_equal @blob, ActiveStorageBlobFinder.find_blob_from_url(redirect_url)
    assert_equal @blob, ActiveStorageBlobFinder.find_blob_from_url(proxy_url)
  end

  test "returns nil for blank url or url without signed id" do
    assert_nil ActiveStorageBlobFinder.find_blob_from_url(nil)
    assert_nil ActiveStorageBlobFinder.find_blob_from_url("")
    assert_nil ActiveStorageBlobFinder.find_blob_from_url("https://example.com/images/photo.png")
  end

  test "returns nil for tampered signed id" do
    url = "/rails/active_storage/blobs/redirect/#{@blob.signed_id}tampered--x9/image.png"

    assert_nil ActiveStorageBlobFinder.find_blob_from_url(url)
  end

  test "returns nil when the signed blob no longer exists" do
    ActiveStorage::Blob.stub(:find_signed, ->(*) { raise ActiveRecord::RecordNotFound }) do
      url = "/rails/active_storage/blobs/redirect/#{@blob.signed_id}/image.png"
      assert_nil ActiveStorageBlobFinder.find_blob_from_url(url)
    end
  end

  test "lets unexpected errors propagate" do
    ActiveStorage::Blob.stub(:find_signed, ->(*) { raise NoMethodError, "boom" }) do
      url = "/rails/active_storage/blobs/redirect/#{@blob.signed_id}/image.png"
      assert_raises(NoMethodError) { ActiveStorageBlobFinder.find_blob_from_url(url) }
    end
  end
end
