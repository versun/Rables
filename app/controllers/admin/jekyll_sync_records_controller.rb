class Admin::JekyllSyncRecordsController < Admin::BaseController
  def index
    @sync_records = JekyllSyncRecord.order(created_at: :desc)
  end
end
