# frozen_string_literal: true

require "test_helper"

class PublishScheduledPagesJobTest < ActiveJob::TestCase
  def create_page(attrs = {})
    Page.create!({
      title: "Scheduled Page",
      slug: "scheduled-page-#{SecureRandom.hex(4)}",
      status: :schedule,
      scheduled_at: 1.hour.from_now,
      content_type: :html,
      html_content: "<p>Page content</p>"
    }.merge(attrs))
  end

  test "publishes scheduled page when time has passed" do
    page = create_page(scheduled_at: 1.hour.ago)

    PublishScheduledPagesJob.perform_now(page.id)

    page.reload
    assert page.publish?
    assert_nil page.scheduled_at
  end

  test "does not publish page scheduled for future" do
    page = create_page

    PublishScheduledPagesJob.perform_now(page.id)

    page.reload
    assert page.schedule?
    assert_not_nil page.scheduled_at
  end

  test "handles missing page gracefully" do
    assert_nothing_raised do
      PublishScheduledPagesJob.perform_now(999999)
    end
  end

  test "schedule_at creates job for scheduled page" do
    page = create_page

    assert_enqueued_with(job: PublishScheduledPagesJob) do
      PublishScheduledPagesJob.schedule_at(page)
    end
  end

  test "schedule_at does nothing for non-scheduled page" do
    page = create_page(status: :draft, scheduled_at: nil)

    assert_no_enqueued_jobs do
      PublishScheduledPagesJob.schedule_at(page)
    end
  end

  test "skips publishing when scheduled_at is nil" do
    page = create_page(status: :draft, scheduled_at: nil)
    # Force a stale state: scheduled status without a scheduled_at time
    page.update_columns(status: Page.statuses[:schedule], scheduled_at: nil)

    assert_nothing_raised do
      PublishScheduledPagesJob.perform_now(page.id)
    end

    page.reload
    assert page.schedule?
  end

  test "schedule_at is enqueued when a page is saved with schedule status" do
    assert_enqueued_with(job: PublishScheduledPagesJob) do
      create_page
    end
  end
end
