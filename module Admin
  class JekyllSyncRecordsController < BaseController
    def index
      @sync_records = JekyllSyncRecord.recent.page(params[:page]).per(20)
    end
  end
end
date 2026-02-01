module Admin
  class JekyllSyncRecordsController < BaseController
    def index
      @sync_records = JekyllSyncRecord.recent.page(params[:page]).per(20)
    end
  end
end
