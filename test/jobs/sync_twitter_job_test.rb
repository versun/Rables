# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class SyncTwitterJobTest < ActiveJob::TestCase
  test "perform delegates to TwitterSyncService" do
    received_force = nil
    service = Object.new
    service.define_singleton_method(:perform) { |force: false| received_force = force }

    TwitterSyncService.stub(:new, service) do
      SyncTwitterJob.perform_now
    end

    assert_equal false, received_force
  end

  test "perform forwards force to TwitterSyncService" do
    received_force = nil
    service = Object.new
    service.define_singleton_method(:perform) { |force: false| received_force = force }

    TwitterSyncService.stub(:new, service) do
      SyncTwitterJob.perform_now(force: true)
    end

    assert_equal true, received_force
  end

  test "job is queued on the default queue" do
    assert_equal "default", SyncTwitterJob.new.queue_name
  end

  test "job limits concurrency to a single execution" do
    assert_equal 1, SyncTwitterJob.concurrency_limit
  end

  test "job holds the concurrency lock long enough for a full sync" do
    assert_equal 1.hour, SyncTwitterJob.concurrency_duration
  end
end
