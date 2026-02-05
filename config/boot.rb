ENV["BUNDLE_GEMFILE"] ||= File.expand_path("../Gemfile", __dir__)

require "bundler/setup" # Set up gems listed in the Gemfile.

if ENV["RAILS_ENV"] == "test" || ENV["RACK_ENV"] == "test" ||
   (File.basename($PROGRAM_NAME) == "rails" && ARGV.first&.start_with?("test"))
  require "simplecov"

  SimpleCov.start "rails"
  SimpleCov.at_exit do
    SimpleCov.result
  end
end

require "bootsnap/setup" # Speed up boot time by caching expensive operations.
