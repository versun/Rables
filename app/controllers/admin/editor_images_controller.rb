class Admin::EditorImagesController < Admin::BaseController
  ALLOWED_CONTENT_TYPES = %w[image/png image/jpeg image/gif image/webp].freeze

  def create
    uploaded_file = params[:file]

    unless uploaded_file
      return render json: { error: "No file provided" }, status: :bad_request
    end

    if uploaded_file.size > 100.megabytes
      return render json: { error: "File too large (max 100MB)" }, status: :bad_request
    end

    # Only allow common web image types; reject SVG and other active content
    unless ALLOWED_CONTENT_TYPES.include?(uploaded_file.content_type)
      return render json: { error: "Unsupported file type" }, status: :unsupported_media_type
    end

    # Create a unique filename
    extension = File.extname(uploaded_file.original_filename).downcase
    filename = "#{SecureRandom.uuid}#{extension}"

    # Store file using ActiveStorage
    blob = ActiveStorage::Blob.create_and_upload!(
      io: uploaded_file.to_io,
      filename: filename,
      content_type: uploaded_file.content_type
    )

    # Generate a permanent URL that never expires
    # Using rails_blob_url which redirects to actual storage
    # This URL is permanent - it uses signed_id but only for lookup, not expiration
    file_url = Rails.application.routes.url_helpers.rails_blob_url(
      blob,
      host: request.host,
      port: request.port != 80 && request.port != 443 ? request.port : nil,
      protocol: request.protocol
    )

    render json: { location: file_url }
  rescue => e
    Rails.logger.error "TinyMCE upload failed: #{e.message}"
    render json: { error: "Upload failed" }, status: :internal_server_error
  end
end
