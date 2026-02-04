class StaticFilesController < ApplicationController
  allow_unauthenticated_access
  def show
    # params[:filename] is an array from wildcard route, join it back to string
    filename = params[:filename].is_a?(Array) ? params[:filename].join("/") : params[:filename]

    # Find by StaticFile filename
    static_file = StaticFile.find_by(filename: filename)

    if static_file&.file&.attached?
      # 公共对象直接跳转服务 URL，避免 302 二次跳转
      blob = static_file.file.blob
      url = blob.service.public? ? blob.url : rails_blob_path(blob)
      redirect_to url, allow_other_host: true
    else
      head :not_found
    end
  end
end
