# frozen_string_literal: true

require "test_helper"

class ActivityLogTest < ActiveSupport::TestCase
  test "track_activity returns most recent 10 for target and enum is defined" do
    assert_includes ActivityLog.levels.keys, "info"

    12.times do |index|
      timestamp = Time.current - index.minutes
      ActivityLog.create!(
        action: "event-#{index}",
        target: "import",
        level: :info,
        created_at: timestamp,
        updated_at: timestamp
      )
    end

    results = ActivityLog.track_activity("import")
    assert_equal 10, results.size
    assert results.each_cons(2).all? { |a, b| a.created_at >= b.created_at }
  end

  test "log! normalizes values and formats description" do
    log = ActivityLog.log!(
      action: "Created",
      target: "Article",
      level: "warning",
      title: "Hello World",
      slug: "hello-world",
      errors: "Example error"
    )

    assert_equal "created", log.action
    assert_equal "article", log.target
    assert_equal "warn", log.level
    assert_equal "title=\"Hello World\" slug=\"hello-world\" errors=\"Example error\"", log.description
  end

  test "log! formats array values with quoting" do
    log = ActivityLog.log!(
      action: :updated,
      target: :tag,
      tags: [ "Ruby on Rails", "Dev" ]
    )

    assert_equal "tags=[\"Ruby on Rails\",\"Dev\"]", log.description
  end

  test "log! defaults invalid levels to warn" do
    log = ActivityLog.log!(
      action: :created,
      target: :article,
      level: "warnng"
    )

    assert_equal "warn", log.level
  end
end
