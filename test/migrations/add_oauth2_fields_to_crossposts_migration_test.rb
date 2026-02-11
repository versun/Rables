# frozen_string_literal: true

require "test_helper"
require Rails.root.join("db/migrate/20260209000000_add_oauth2_fields_to_crossposts")

class AddOauth2FieldsToCrosspostsMigrationTest < ActiveSupport::TestCase
  test "down removes refresh_token when present" do
    migration = AddOauth2FieldsToCrossposts.new
    removed_columns = []

    migration.define_singleton_method(:column_exists?) do |_table, column|
      %i[refresh_token client_id token_expires_at].include?(column)
    end
    migration.define_singleton_method(:add_column) { |_table, _column, _type| true }
    migration.define_singleton_method(:remove_column) do |table, column, type|
      removed_columns << [ table, column, type ]
    end

    migration.down

    assert_includes removed_columns, [ :crossposts, :refresh_token, :string ]
  end
end
