# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class ListmonkSenderJobTest < ActiveJob::TestCase
  test "logs start and skips when listmonk is not configured" do
    article = create_published_article

    assert_difference "ActivityLog.count", 1 do
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
end
