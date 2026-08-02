class Admin::StaticFilesController < Admin::BaseController
  def index
    @static_files = StaticFile.includes(file_attachment: :blob).order(created_at: :desc)
  end

  def create
    uploaded_file = params.dig(:static_file, :file)

    unless uploaded_file
      @static_files = StaticFile.includes(file_attachment: :blob).order(created_at: :desc)
      flash.now[:alert] = "请选择要上传的文件"
      render :index
      return
    end

    # 首先获取新附件的文件名
    new_filename = uploaded_file.original_filename

    # 搜索是否已有记录
    existing_file = StaticFile.find_by(filename: new_filename)

    if existing_file
      # Update the existing record in place, keeping its original filename
      if existing_file.update(static_file_params.except(:filename))
        ActivityLog.log!(
          action: :updated,
          target: :static_file,
          level: :info,
          filename: new_filename
        )
        redirect_to admin_static_files_path, notice: "文件上传成功（已覆盖同名文件）"
      else
        ActivityLog.log!(
          action: :failed,
          target: :static_file,
          level: :error,
          filename: new_filename,
          errors: existing_file.errors.full_messages.join(", ")
        )
        @static_files = StaticFile.includes(file_attachment: :blob).order(created_at: :desc)
        flash.now[:alert] = "文件上传失败: #{existing_file.errors.full_messages.join(', ')}"
        render :index
      end
    else
      # 如果没有记录，则创建新记录
      @static_file = StaticFile.new(static_file_params.merge(filename: new_filename))

      if @static_file.save
        ActivityLog.log!(
          action: :created,
          target: :static_file,
          level: :info,
          filename: new_filename
        )
        redirect_to admin_static_files_path, notice: "文件上传成功"
      else
        ActivityLog.log!(
          action: :failed,
          target: :static_file,
          level: :error,
          filename: new_filename,
          errors: @static_file.errors.full_messages.join(", ")
        )
        @static_files = StaticFile.includes(file_attachment: :blob).order(created_at: :desc)
        flash.now[:alert] = "文件上传失败: #{@static_file.errors.full_messages.join(', ')}"
        render :index
      end
    end
  end

  def destroy
    @static_file = StaticFile.find(params[:id])
    filename = @static_file.filename

    if @static_file.destroy
      ActivityLog.log!(
        action: :deleted,
        target: :static_file,
        level: :info,
        filename: filename
      )
      redirect_to admin_static_files_path, notice: "文件 #{filename} 已删除"
    else
      ActivityLog.log!(
        action: :failed,
        target: :static_file,
        level: :error,
        filename: filename
      )
      redirect_to admin_static_files_path, alert: "删除失败"
    end
  end

  private

  def static_file_params
    params.require(:static_file).permit(:description, :file, :filename)
  end
end
