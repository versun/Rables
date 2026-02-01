# frozen_string_literal: true

module Admin
  class JekyllSyncRecordsController < BaseController
    def index
      @sync_records = JekyllSyncRecord.recent.paginate(page: params[:page], per_page: 20)
    end
  end
end
