class TwitterSync < ApplicationRecord
  SCHEDULES = {
    "every_15_minutes" => 15.minutes,
    "hourly" => 1.hour,
    "every_6_hours" => 6.hours,
    "daily" => 1.day,
    "weekly" => 1.week
  }.freeze

  before_validation :normalize_username

  validates :username, presence: true, if: :enabled?
  validates :sync_schedule, inclusion: { in: SCHEDULES.keys }

  def self.instance
    first_or_create
  end

  def due_to_sync?(now = Time.current)
    return true if last_synced_at.nil?

    interval = SCHEDULES.fetch(sync_schedule, SCHEDULES["every_15_minutes"])
    last_synced_at < now - interval
  end

  private

  def normalize_username
    self.username = username.to_s.strip.sub(/\A@/, "").presence
  end
end
