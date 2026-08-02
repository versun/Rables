# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class ListmonkSenderJobTest < ActiveJob::TestCase
  test "skips without logging when listmonk is not configured" do
    article = create_published_article

    assert_no_difference "ActivityLog.count" do
      ListmonkSenderJob.perform_now(article.id)
    end
  end

  test "sends newsletter when listmonk is configured" do
    article = create_published_article

    mock_listmonk = Minitest::Mock.new
    mock_listmonk.expect(:present?, true)
    mock_listmonk.expect(:list_id, "1")
    mock_listmonk.expect(:template_id, "2")
    mock_listmonk.expect(:send_newsletter, true, [ article, "Test Site" ])

    Listmonk.stub(:first, mock_listmonk) do
      ListmonkSenderJob.perform_now(article.id)
    end

    assert mock_listmonk.verify
  end

  test "logs failure and re-raises when sending raises unexpectedly" do
    article = create_published_article

    failing_listmonk = Object.new
    failing_listmonk.define_singleton_method(:present?) { true }
    failing_listmonk.define_singleton_method(:list_id) { "1" }
    failing_listmonk.define_singleton_method(:template_id) { "2" }
    failing_listmonk.define_singleton_method(:send_newsletter) { |*| raise StandardError, "boom" }

    Listmonk.stub(:first, failing_listmonk) do
      # started + failed logs
      assert_difference "ActivityLog.count", 2 do
        assert_raises(StandardError) { ListmonkSenderJob.perform_now(article.id) }
      end
    end

    log = ActivityLog.last
    assert_equal "failed", log.action
    assert_equal "newsletter", log.target
    assert_includes log.description, "boom"
  end

  test "handles missing article gracefully" do
    assert_no_difference "ActivityLog.count" do
      assert_nothing_raised do
        ListmonkSenderJob.perform_now(999999)
      end
    end
  end
end
