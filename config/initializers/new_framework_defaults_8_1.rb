# Be sure to restart your server when you modify this file.
#
# This file enables Rails 8.1 framework defaults.
# Read the Guide for Upgrading Ruby on Rails for more info on each option.
# https://guides.rubyonrails.org/upgrading_ruby_on_rails.html

# Skips escaping HTML entities and line separators for better performance
Rails.configuration.action_controller.escape_json_responses = false

# Skips escaping LINE SEPARATOR and PARAGRAPH SEPARATOR in JSON (safe in modern browsers)
Rails.configuration.active_support.escape_js_separators_in_json = false

# Raises error when order dependent finder methods are called without order values
Rails.configuration.active_record.raise_on_missing_required_finder_order_columns = true

# Raise error for path relative URL redirects to prevent open redirect vulnerabilities
Rails.configuration.action_controller.action_on_path_relative_redirect = :raise

# Use Ruby parser to track dependencies between Action View templates
Rails.configuration.action_view.render_tracker = :ruby

# Hidden inputs omit autocomplete="off" attribute
Rails.configuration.action_view.remove_hidden_field_autocomplete = true
