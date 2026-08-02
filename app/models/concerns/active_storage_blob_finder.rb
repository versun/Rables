module ActiveStorageBlobFinder
  module_function

  # 从 ActiveStorage URL 中查找 Blob
  # 匹配 /rails/active_storage/blobs/redirect/:signed_id/:filename
  # 或 /rails/active_storage/blobs/proxy/:signed_id/:filename
  # 或 /rails/active_storage/blobs/:signed_id/:filename (旧格式)
  # 或 /rails/active_storage/representations/.../:signed_blob_id/...
  # signed_id 格式: message--signature (URL-safe base64 包含 A-Z, a-z, 0-9, -, _, = 和 -- 分隔符)
  def find_blob_from_url(url)
    return nil if url.blank?

    signed_id = extract_signed_id(url)
    return nil if signed_id.blank?

    ActiveStorage::Blob.find_signed(signed_id)
  rescue ActiveRecord::RecordNotFound, ActiveSupport::MessageVerifier::InvalidSignature => e
    # Expected lookup failures: a validly-signed id whose blob was deleted, or a
    # tampered/expired signature. Anything else (DB errors, bugs) should raise.
    Rails.logger.warn "Failed to find blob from url: #{url}, error: #{e.message}"
    nil
  end

  def extract_signed_id(url)
    if url =~ %r{/rails/active_storage/blobs/[^/]+/([A-Za-z0-9_-]+(?:={0,2})--[A-Za-z0-9_-]+)}
      ::Regexp.last_match(1)
    elsif url =~ %r{/rails/active_storage/representations/[^/]+/([^/]+)}
      ::Regexp.last_match(1)
    end
  end
end
