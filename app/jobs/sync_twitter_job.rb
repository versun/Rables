# Recurring job (via config/recurring.yml) that archives new tweets from the
# configured Twitter/X account as articles. Kept thin so it can be re-run
# manually from Mission Control.
#
# The recurring schedule fires every 15 minutes; the job itself checks the
# configured sync_schedule and skips when it is not due yet. Manual triggers
# (admin "Sync Now") pass force: true to bypass that check.
#
# Concurrency is limited to 1 so a manual "Sync Now" can never overlap with a
# recurring run (which would surface as spurious slug-uniqueness errors).
class SyncTwitterJob < ApplicationJob
  queue_as :default

  limits_concurrency to: 1, key: "twitter_sync", duration: 1.hour

  def perform(force: false)
    TwitterSyncService.new.perform(force: force)
  end
end
