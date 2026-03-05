class AddOauth2FieldsToCrossposts < ActiveRecord::Migration[8.1]
  class CrosspostRecord < ActiveRecord::Base
    self.table_name = "crossposts"
  end

  def up
    legacy_oauth1_columns_present = column_exists?(:crossposts, :access_token_secret) ||
      column_exists?(:crossposts, :api_key) ||
      column_exists?(:crossposts, :api_key_secret)

    # Do not migrate OAuth 1.0a secrets into OAuth 2.0 refresh tokens.
    if column_exists?(:crossposts, :access_token_secret)
      add_column :crossposts, :refresh_token, :string unless column_exists?(:crossposts, :refresh_token)
      remove_column :crossposts, :access_token_secret, :string
    end

    add_column :crossposts, :client_id, :string unless column_exists?(:crossposts, :client_id)
    add_column :crossposts, :token_expires_at, :datetime unless column_exists?(:crossposts, :token_expires_at)

    # Twitter crosspost auth is OAuth 2.0 only from this migration onward.
    remove_column :crossposts, :api_key, :string if column_exists?(:crossposts, :api_key)
    remove_column :crossposts, :api_key_secret, :string if column_exists?(:crossposts, :api_key_secret)

    return unless legacy_oauth1_columns_present
    return unless table_exists?(:crossposts)
    return unless column_exists?(:crossposts, :platform)

    say_with_time "Disabling legacy Twitter crosspost credentials" do
      updates = { enabled: false }
      updates[:access_token] = nil if column_exists?(:crossposts, :access_token)
      updates[:refresh_token] = nil if column_exists?(:crossposts, :refresh_token)
      updates[:token_expires_at] = nil if column_exists?(:crossposts, :token_expires_at)

      CrosspostRecord.where(platform: "twitter").update_all(updates)
    end
  end

  def down
    add_column :crossposts, :api_key, :string unless column_exists?(:crossposts, :api_key)
    add_column :crossposts, :api_key_secret, :string unless column_exists?(:crossposts, :api_key_secret)
    add_column :crossposts, :access_token_secret, :string unless column_exists?(:crossposts, :access_token_secret)

    remove_column :crossposts, :refresh_token, :string if column_exists?(:crossposts, :refresh_token)
    remove_column :crossposts, :client_id, :string if column_exists?(:crossposts, :client_id)
    remove_column :crossposts, :token_expires_at, :datetime if column_exists?(:crossposts, :token_expires_at)
  end
end
