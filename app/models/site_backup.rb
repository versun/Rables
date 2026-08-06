# frozen_string_literal: true

require "fileutils"
require "pathname"
require "securerandom"
require "zip"

# Full-site backup: a ZIP archive holding a snapshot of the primary SQLite
# database (database.sqlite3) plus every uploaded file from the local
# ActiveStorage disk service (storage/).
#
# Importing a backup REPLACES the entire current database (via the SQLite
# online backup API, with a rollback copy taken first) and merges the
# archived files into the storage root.
class SiteBackup
  class Error < StandardError; end

  Zip.unicode_names = true
  Zip.force_entry_names_encoding = "UTF-8"

  DATABASE_ENTRY = "database.sqlite3"
  STORAGE_ENTRY = "storage"
  SQLITE_MAGIC = "SQLite format 3\x00".b
  # Production keeps the SQLite databases in the same directory as the
  # ActiveStorage blobs; never copy them into a backup or back out of one.
  SQLITE_FILE_PATTERN = /\A[^.]+\.sqlite3(-.*)?\z/

  def self.export(**options)
    new(**options).export
  end

  def self.import(zip_path, **options)
    new(**options).import(zip_path)
  end

  def initialize(db_path: self.class.default_db_path, storage_root: self.class.default_storage_root, tmp_dir: Rails.root.join("tmp"), finalize: true)
    @db_path = Pathname.new(db_path)
    @storage_root = storage_root && Pathname.new(storage_root)
    @exports_dir = Pathname.new(tmp_dir).join("exports")
    @imports_dir = Pathname.new(tmp_dir).join("imports")
    @finalize = finalize
  end

  def self.default_db_path
    database = ActiveRecord::Base.connection_db_config.database
    Pathname.new(database).absolute? ? database : Rails.root.join(database).to_s
  end

  def self.default_storage_root
    service = ActiveStorage::Blob.service
    service.is_a?(ActiveStorage::Service::DiskService) ? service.root : nil
  end

  # Builds the backup ZIP and returns its path. Old archives are purged by
  # CleanOldExportsJob.
  def export
    timestamp = Time.current.strftime("%Y%m%d_%H%M%S")
    work_dir = @exports_dir.join("backup_#{timestamp}_#{SecureRandom.hex(4)}")
    zip_path = @exports_dir.join("rables_backup_#{timestamp}_#{SecureRandom.hex(4)}.zip")
    FileUtils.mkdir_p(work_dir)

    snapshot_database(work_dir.join(DATABASE_ENTRY))
    copy_storage(work_dir.join(STORAGE_ENTRY))
    create_zip(work_dir, zip_path)
    zip_path
  rescue StandardError
    FileUtils.rm_f(zip_path) if zip_path
    raise
  ensure
    FileUtils.rm_rf(work_dir) if work_dir
  end

  def import(zip_path)
    zip_path = Pathname.new(zip_path)
    validate_zip!(zip_path)

    work_dir = @imports_dir.join("restore_#{Time.current.to_i}_#{SecureRandom.hex(8)}")
    FileUtils.mkdir_p(work_dir)

    begin
      extract_zip(zip_path, work_dir)
      database_file = work_dir.join(DATABASE_ENTRY)
      validate_database!(database_file)
      restore_storage(work_dir.join(STORAGE_ENTRY))
      restore_database(database_file, work_dir)
      finalize! if @finalize
      true
    ensure
      FileUtils.rm_rf(work_dir)
    end
  end

  private

  # --- Export -------------------------------------------------------------

  def snapshot_database(destination)
    raise Error, "Database file not found: #{@db_path}" unless @db_path.file?

    quoted = destination.to_s.gsub("'", "''")
    db = SQLite3::Database.new(@db_path.to_s)
    begin
      db.busy_timeout = 5000
      db.execute("VACUUM INTO '#{quoted}'")
    ensure
      db.close
    end
  rescue SQLite3::Exception => e
    FileUtils.rm_f(destination)
    raise Error, "Database snapshot failed: #{e.message}"
  end

  def copy_storage(destination)
    return unless @storage_root&.directory?

    FileUtils.mkdir_p(destination)
    @storage_root.each_child do |entry|
      next if entry.file? && entry.basename.to_s.match?(SQLITE_FILE_PATTERN)

      FileUtils.cp_r(entry, destination, preserve: true)
    end
  end

  def create_zip(source_dir, zip_path)
    Zip::OutputStream.open(zip_path.to_s) do |zos|
      Dir.glob(source_dir.join("**", "*")).sort.each do |file|
        next unless File.file?(file)

        relative_path = Pathname.new(file).relative_path_from(source_dir).to_s
        relative_path = relative_path.tr("\\", "/")
        relative_path = relative_path.encode("UTF-8", invalid: :replace, undef: :replace, replace: "_")

        zos.put_next_entry(relative_path)
        File.open(file, "rb") { |f| IO.copy_stream(f, zos) }
      end
    end
  end

  # --- Import -------------------------------------------------------------

  def validate_zip!(zip_path)
    raise Error, "Backup file not found" unless zip_path.file?

    entries = begin
      Zip::File.open(zip_path.to_s) { |zip_file| zip_file.entries.map(&:name) }
    rescue Zip::Error
      nil
    end

    raise Error, "Invalid ZIP file" unless entries
    # A database.sqlite3-wal/-shm stowed in the archive would be replayed by
    # SQLite the moment the extracted file is opened, bypassing validation.
    if entries.any? { |name| name.start_with?("#{DATABASE_ENTRY}-") }
      raise Error, "Backup archive contains unexpected database sidecar files"
    end
    raise Error, "Backup archive is missing #{DATABASE_ENTRY}" unless entries.include?(DATABASE_ENTRY)
  end

  def extract_zip(zip_path, destination)
    destination = destination.expand_path
    Zip::File.open(zip_path.to_s) do |zip_file|
      zip_file.each do |entry|
        next if entry.directory?

        target = destination.join(entry.name).expand_path
        unless target.to_s.start_with?(destination.to_s + File::SEPARATOR)
          raise Error, "Unsafe entry in archive: #{entry.name}"
        end

        FileUtils.mkdir_p(target.dirname)
        entry.extract(entry.name, destination_directory: destination.to_s)
      end
    end
  end

  def validate_database!(database_file)
    raise Error, "Backup archive is missing #{DATABASE_ENTRY}" unless database_file.file?

    magic = File.open(database_file, "rb") { |f| f.read(SQLITE_MAGIC.bytesize) }
    raise Error, "Backup is not a SQLite database" unless magic == SQLITE_MAGIC

    db = SQLite3::Database.new(database_file.to_s, readonly: true)
    begin
      result = db.execute("PRAGMA quick_check")
      raise Error, "SQLite database failed integrity check" unless result.first&.first.to_s == "ok"
    ensure
      db.close
    end
  rescue SQLite3::Exception => e
    raise Error, "Invalid SQLite database: #{e.message}"
  end

  def restore_storage(archive_storage_dir)
    return unless archive_storage_dir.directory?

    unless @storage_root
      raise Error, "Active Storage is not using the local disk service; the database was left untouched, restore the archived files manually"
    end

    FileUtils.mkdir_p(@storage_root)
    # Never restore SQLite files over the live databases sitting in the
    # storage root (see SQLITE_FILE_PATTERN).
    children = archive_storage_dir.children.reject do |entry|
      entry.file? && entry.basename.to_s.match?(SQLITE_FILE_PATTERN)
    end
    FileUtils.cp_r(children, @storage_root, preserve: true)
  end

  # Replaces the live database with the archived one through the SQLite
  # online backup API. A copy of the current database is taken first so a
  # failed restore can be rolled back.
  def restore_database(database_file, work_dir)
    rollback_file = work_dir.join("rollback.sqlite3")
    snapshot_taken = false
    snapshot_database(rollback_file)
    snapshot_taken = true
    copy_database(from: database_file, to: @db_path)
  rescue StandardError => e
    raise e unless snapshot_taken

    rollback_with(rollback_file, e)
  end

  def rollback_with(rollback_file, original_error)
    copy_database(from: rollback_file, to: @db_path)
  rescue StandardError => rollback_error
    # The work dir is deleted right after this; move the last known-good
    # copy of the database somewhere durable before giving up.
    preserved = @imports_dir.join("rollback_#{Time.current.strftime("%Y%m%d_%H%M%S")}_#{SecureRandom.hex(4)}.sqlite3")
    FileUtils.mv(rollback_file, preserved)
    raise Error, "Restore failed (#{original_error.message}) and rollback also failed (#{rollback_error.message}). A copy of the previous database was preserved at #{preserved}"
  else
    raise Error, "Restore failed; the previous database was rolled back: #{original_error.message}"
  end

  def copy_database(from:, to:)
    source = SQLite3::Database.new(from.to_s, readonly: true)
    dest = SQLite3::Database.new(to.to_s)
    begin
      dest.busy_timeout = 5000
      backup = SQLite3::Backup.new(dest, "main", source, "main")
      status = backup.step(-1)
      backup.finish
      unless status == SQLite3::Constants::ErrorCode::DONE
        status_name = SQLite3::Constants::ErrorCode.constants.find { |c| SQLite3::Constants::ErrorCode.const_get(c) == status }
        raise SQLite3::Exception, "database copy failed (SQLite status #{status_name || status})"
      end
    ensure
      source.close
      dest.close
    end
  rescue SQLite3::Exception => e
    raise Error, e.message
  end

  def finalize!
    ActiveRecord::Tasks::DatabaseTasks.migrate_all
  rescue StandardError => e
    raise Error, "Backup was restored but running migrations failed: #{e.message}. Run `bin/rails db:migrate` and restart the server."
  ensure
    ActiveRecord::Base.connection_handler.clear_all_connections!
    begin
      Rails.cache.clear
    rescue StandardError => e
      Rails.logger.warn("SiteBackup: cache clear failed: #{e.message}")
    end
  end
end
