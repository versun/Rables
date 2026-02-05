# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class ImportFromRssJobTest < ActiveJob::TestCase
  test "delegates import to ImportRss" do
    mock_importer = Minitest::Mock.new
    mock_importer.expect(:import_data, true)

    ImportRss.stub(:new, mock_importer) do
      ImportFromRssJob.perform_now("https://example.com/feed.xml", true)
    end

    assert mock_importer.verify
  end
end
