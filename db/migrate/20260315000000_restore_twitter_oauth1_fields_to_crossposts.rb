class RestoreTwitterOauth1FieldsToCrossposts < ActiveRecord::Migration[8.1]
  def change
    add_column :crossposts, :api_key, :string unless column_exists?(:crossposts, :api_key)
    add_column :crossposts, :api_key_secret, :string unless column_exists?(:crossposts, :api_key_secret)
    add_column :crossposts, :access_token_secret, :string unless column_exists?(:crossposts, :access_token_secret)
  end
end
