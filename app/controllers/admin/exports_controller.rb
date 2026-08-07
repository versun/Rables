class Admin::ExportsController < Admin::BaseController
  def download
    export = Export.find(params[:id])

    if export.file_available?
      send_file export.file_path,
                filename: export.filename,
                type: "application/zip",
                disposition: "attachment"
    else
      redirect_to admin_migrates_path(tab: "export"),
        alert: "Export file is not available (status: #{export.status})."
    end
  end
end
