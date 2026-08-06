# frozen_string_literal: true

require "test_helper"
require "zip"

class SiteBackupTest < ActiveSupport::TestCase
  setup do
    @tmp = Dir.mktmpdir("site_backup_test")
    @source_db = create_database(File.join(@tmp, "source.sqlite3"), "from source")
    @storage_root = File.join(@tmp, "storage")
    populate_storage(@storage_root)
  end

  teardown do
    FileUtils.rm_rf(@tmp)
  end

  test "export creates a zip containing the database snapshot and storage files" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)

    entries = zip_entries(zip_path)
    assert_includes entries, "database.sqlite3"
    assert_includes entries, "storage/ab/cd/abcdef123456"
  end

  test "export excludes top-level sqlite files from the storage root" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)

    entries = zip_entries(zip_path)
    assert_equal [ "database.sqlite3" ], entries.grep(/sqlite3/)
  end

  test "export snapshot contains the source database content" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)

    extract_dir = File.join(@tmp, "extracted")
    FileUtils.mkdir_p(extract_dir)
    Zip::File.open(zip_path.to_s) do |zip_file|
      zip_file.extract("database.sqlite3", "database.sqlite3", destination_directory: extract_dir)
    end

    assert_equal [ "from source" ], read_titles(File.join(extract_dir, "database.sqlite3"))
  end

  test "import replaces the live database and merges storage files" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)

    live_db = create_database(File.join(@tmp, "live.sqlite3"), "old data")
    live_storage = File.join(@tmp, "live_storage")
    FileUtils.mkdir_p(File.join(live_storage, "zz"))
    File.write(File.join(live_storage, "zz", "stale"), "keep me")

    SiteBackup.import(zip_path, db_path: live_db, storage_root: live_storage, tmp_dir: @tmp, finalize: false)

    assert_equal [ "from source" ], read_titles(live_db)
    assert_equal "blob-data", File.read(File.join(live_storage, "ab", "cd", "abcdef123456"))
    assert_equal "keep me", File.read(File.join(live_storage, "zz", "stale"))
  end

  test "import never restores sqlite files over the storage root" do
    zip_path = File.join(@tmp, "crafted.zip")
    Zip::File.open(zip_path, create: true) do |zip_file|
      zip_file.add("database.sqlite3", @source_db)
      zip_file.get_output_stream("storage/production.sqlite3") { |f| f.write "malicious" }
      zip_file.get_output_stream("storage/production_queue.sqlite3-wal") { |f| f.write "malicious" }
    end

    live_db = create_database(File.join(@tmp, "live.sqlite3"), "old data")
    live_storage = File.join(@tmp, "live_storage")
    FileUtils.mkdir_p(live_storage)
    File.write(File.join(live_storage, "production.sqlite3"), "do not touch")
    File.write(File.join(live_storage, "production_queue.sqlite3-wal"), "do not touch")

    SiteBackup.import(zip_path, db_path: live_db, storage_root: live_storage, tmp_dir: @tmp, finalize: false)

    assert_equal "do not touch", File.read(File.join(live_storage, "production.sqlite3"))
    assert_equal "do not touch", File.read(File.join(live_storage, "production_queue.sqlite3-wal"))
    assert_equal [ "from source" ], read_titles(live_db)
  end

  test "import rejects a non-zip file" do
    bad_file = File.join(@tmp, "bad.zip")
    File.write(bad_file, "this is not a zip")

    error = assert_raises(SiteBackup::Error) do
      SiteBackup.import(bad_file, db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp, finalize: false)
    end
    assert_match "ZIP", error.message
  end

  test "import rejects a zip without database.sqlite3" do
    zip_path = File.join(@tmp, "no_database.zip")
    Zip::File.open(zip_path, create: true) do |zip_file|
      zip_file.get_output_stream("readme.txt") { |f| f.write "no database here" }
    end

    error = assert_raises(SiteBackup::Error) do
      SiteBackup.import(zip_path, db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp, finalize: false)
    end
    assert_match "database.sqlite3", error.message
  end

  test "import rejects archives with database sidecar entries" do
    zip_path = File.join(@tmp, "sidecar.zip")
    Zip::File.open(zip_path, create: true) do |zip_file|
      zip_file.add("database.sqlite3", @source_db)
      zip_file.get_output_stream("database.sqlite3-wal") { |f| f.write "unvalidated pages" }
    end

    error = assert_raises(SiteBackup::Error) do
      SiteBackup.import(zip_path, db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp, finalize: false)
    end
    assert_match "sidecar", error.message
  end

  test "import rejects archives with unsafe entry paths" do
    zip_path = File.join(@tmp, "evil.zip")
    Zip::File.open(zip_path, create: true) do |zip_file|
      zip_file.add("database.sqlite3", @source_db)
      zip_file.get_output_stream("../evil.txt") { |f| f.write "pwned" }
    end

    error = assert_raises(SiteBackup::Error) do
      SiteBackup.import(zip_path, db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp, finalize: false)
    end
    assert_match "Unsafe entry", error.message
    assert_not File.exist?(File.join(@tmp, "evil.txt"))
  end

  test "import rejects a corrupted database file" do
    zip_path = File.join(@tmp, "corrupt.zip")
    Zip::File.open(zip_path, create: true) do |zip_file|
      zip_file.get_output_stream("database.sqlite3") { |f| f.write "garbage-bytes-not-sqlite" }
    end

    error = assert_raises(SiteBackup::Error) do
      SiteBackup.import(zip_path, db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp, finalize: false)
    end
    assert_match "SQLite", error.message
  end

  test "import fails before touching the database when storage cannot be restored" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)
    live_db = create_database(File.join(@tmp, "live.sqlite3"), "old data")

    error = assert_raises(SiteBackup::Error) do
      SiteBackup.import(zip_path, db_path: live_db, storage_root: nil, tmp_dir: @tmp, finalize: false)
    end
    assert_match "disk service", error.message
    assert_equal [ "old data" ], read_titles(live_db)
  end

  test "import rolls back the live database when the restore fails midway" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)
    live_db = create_database(File.join(@tmp, "live.sqlite3"), "old data")

    backup = SiteBackup.new(db_path: live_db, storage_root: File.join(@tmp, "live_storage"), tmp_dir: @tmp, finalize: false)
    fail_once = Module.new do
      def copy_database(from:, to:)
        if @restore_should_fail
          @restore_should_fail = false
          # Simulate a restore that dies after partially writing the live DB:
          # the file stays a valid database but the content is wrong.
          db = SQLite3::Database.new(to.to_s)
          db.execute("DELETE FROM articles")
          db.execute("INSERT INTO articles (title) VALUES ('partial junk')")
          db.close
          raise SQLite3::Exception, "simulated restore failure"
        end
        super
      end
    end
    backup.instance_variable_set(:@restore_should_fail, true)
    backup.singleton_class.prepend(fail_once)

    error = assert_raises(SiteBackup::Error) { backup.import(zip_path) }
    assert_match "rolled back", error.message
    assert_equal [ "old data" ], read_titles(live_db)
  end

  test "import preserves the rollback copy when restore and rollback both fail" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)
    live_db = create_database(File.join(@tmp, "live.sqlite3"), "old data")

    backup = SiteBackup.new(db_path: live_db, storage_root: File.join(@tmp, "live_storage"), tmp_dir: @tmp, finalize: false)
    always_fail = Module.new do
      def copy_database(from:, to:)
        raise SQLite3::Exception, "always fails"
      end
    end
    backup.singleton_class.prepend(always_fail)

    error = assert_raises(SiteBackup::Error) { backup.import(zip_path) }
    assert_match "rollback also failed", error.message
    assert_match "preserved at", error.message

    preserved = Dir.glob(File.join(@tmp, "imports", "rollback_*.sqlite3"))
    assert_equal 1, preserved.size
    assert_equal [ "old data" ], read_titles(preserved.first)
  end

  test "import raises when the database snapshot cannot be taken" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)
    FileUtils.mkdir_p(File.join(@tmp, "a_directory"))

    assert_raises(SiteBackup::Error) do
      SiteBackup.import(zip_path, db_path: File.join(@tmp, "a_directory"), storage_root: @storage_root, tmp_dir: @tmp, finalize: false)
    end
  end

  test "import with finalize runs migrations, clears connections and clears the cache" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)
    live_db = create_database(File.join(@tmp, "live.sqlite3"), "old data")
    calls = []

    ActiveRecord::Tasks::DatabaseTasks.stub(:migrate_all, ->(*) { calls << :migrate }) do
      ActiveRecord::Base.connection_handler.stub(:clear_all_connections!, ->(*) { calls << :clear_connections }) do
        Rails.cache.stub(:clear, ->(*) { calls << :clear_cache }) do
          SiteBackup.import(zip_path, db_path: live_db, storage_root: File.join(@tmp, "live_storage"), tmp_dir: @tmp, finalize: true)
        end
      end
    end

    assert_equal [ :migrate, :clear_connections, :clear_cache ], calls
    assert_equal [ "from source" ], read_titles(live_db)
  end

  test "import reports migration failures while keeping the restored data" do
    zip_path = SiteBackup.export(db_path: @source_db, storage_root: @storage_root, tmp_dir: @tmp)
    live_db = create_database(File.join(@tmp, "live.sqlite3"), "old data")
    calls = []

    ActiveRecord::Tasks::DatabaseTasks.stub(:migrate_all, ->(*) { raise StandardError, "migration exploded" }) do
      ActiveRecord::Base.connection_handler.stub(:clear_all_connections!, ->(*) { calls << :clear_connections }) do
        Rails.cache.stub(:clear, ->(*) { calls << :clear_cache }) do
          error = assert_raises(SiteBackup::Error) do
            SiteBackup.import(zip_path, db_path: live_db, storage_root: File.join(@tmp, "live_storage"), tmp_dir: @tmp, finalize: true)
          end
          assert_match "running migrations failed", error.message
        end
      end
    end

    assert_equal [ "from source" ], read_titles(live_db)
    assert_includes calls, :clear_connections
    assert_includes calls, :clear_cache
  end

  private

  def create_database(path, marker)
    db = SQLite3::Database.new(path)
    db.execute("CREATE TABLE articles (id INTEGER PRIMARY KEY, title TEXT)")
    db.execute("INSERT INTO articles (title) VALUES (?)", marker)
    db.close
    path
  end

  def read_titles(path)
    db = SQLite3::Database.new(path, readonly: true)
    titles = db.execute("SELECT title FROM articles ORDER BY id").flatten
    db.close
    titles
  end

  def populate_storage(root)
    FileUtils.mkdir_p(File.join(root, "ab", "cd"))
    File.write(File.join(root, "ab", "cd", "abcdef123456"), "blob-data")
    # Production keeps its SQLite databases next to the ActiveStorage blobs;
    # these must never end up in a backup.
    File.write(File.join(root, "production.sqlite3"), "db")
    File.write(File.join(root, "production_queue.sqlite3-wal"), "wal")
  end

  def zip_entries(zip_path)
    Zip::File.open(zip_path.to_s) { |zip_file| zip_file.entries.map(&:name) }
  end
end
