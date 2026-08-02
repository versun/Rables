class TwitterSync < ApplicationRecord
  SCHEDULES = {
    "every_15_minutes" => 15.minutes,
    "hourly" => 1.hour,
    "every_6_hours" => 6.hours,
    "daily" => 1.day,
    "weekly" => 1.week
  }.freeze

  normalizes :username, with: ->(name) { name.to_s.strip.sub(/\A@/, "").presence }

  validates :username, presence: true, if: :enabled?
  validates :sync_schedule, inclusion: { in: SCHEDULES.keys }

  def self.instance
    first_or_create
  rescue ActiveRecord::RecordNotUnique
    # Another process won the create race; use the record it created
    first || retry
  end

  def due_to_sync?
    return true if last_synced_at.nil?

    interval = SCHEDULES.fetch(sync_schedule, SCHEDULES["every_15_minutes"])
    last_synced_at < Time.current - interval
  end
end
