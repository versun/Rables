# frozen_string_literal: true

require "test_helper"

class TwitterArchiveImportTest < ActiveSupport::TestCase
  test "remains valid when active_slot support is unavailable" do
    import = TwitterArchiveImport.new(
      source_filename: "twitter-archive.zip",
      status: "queued",
      progress: 0,
      queued_at: Time.current
    )

    import.define_singleton_method(:has_attribute?) do |name|
      return false if name.to_s == "active_slot"

      super(name)
    end
    import.define_singleton_method(:active_slot=) do |_value|
      raise NoMethodError, "undefined method 'active_slot='"
    end

    assert_predicate import, :valid?
  end
end
