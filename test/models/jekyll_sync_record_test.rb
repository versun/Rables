# frozen_string_literal: true

require "test_helper"

class JekyllSyncRecordTest < ActiveSupport::TestCase
  test "accepts valid enums" do
    record = JekyllSyncRecord.new(sync_type: "full", status: "pending")
    assert record.valid?
  end

  test "rejects invalid enums" do
    record = JekyllSyncRecord.new(sync_type: "nope", status: "bad")
    refute record.valid?
    assert record.errors[:sync_type].any?
    assert record.errors[:status].any?
  end
end
