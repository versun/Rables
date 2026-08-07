class CreateExports < ActiveRecord::Migration[8.1]
  def change
    create_table :exports do |t|
      t.string :kind, null: false
      t.integer :status, null: false, default: 0
      t.string :filename
      t.bigint :byte_size
      t.text :error

      t.timestamps
    end
  end
end
