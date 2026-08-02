# frozen_string_literal: true

require "test_helper"
require "minitest/mock"
require "zip"

class TwitterArchiveImportSubmissionTest < ActiveSupport::TestCase
  setup do
    TwitterArchiveImport.delete_all
  end

  teardown do
    ActiveStorage::Blob.unattached.find_each(&:purge)
  end

  test "submit persists uploaded bytes and queues an import" do
    uploaded_file, original_path = uploaded_zip_file(contents: "archive-bytes")

    result = nil

    assert_difference("TwitterArchiveImport.count", 1) do
      assert_enqueued_with(job: TwitterArchiveImportJob) do
        result = TwitterArchiveImportSubmission.new(uploaded_file).submit
      end
    end

    assert_predicate result, :success?
    assert_equal TwitterArchiveImportSubmission::SUCCESS_NOTICE, result.notice

    import = TwitterArchiveImport.last
    assert_equal "queued", import.status
    assert_equal "archive-bytes", File.binread(import.source_path)
    assert_not_equal original_path, import.source_path
  ensure
    uploaded_file&.tempfile&.close!
    TwitterArchiveImport.last&.cleanup_source_file!
  end

  test "submit copies upload bytes when tempfile move fallback is needed" do
    uploaded_file, original_path = uploaded_zip_file(contents: "archive-bytes")
    submission = TwitterArchiveImportSubmission.new(uploaded_file)
    submission.define_singleton_method(:move_uploaded_tempfile) { |_source, _destination| false }

    result = nil

    assert_difference("TwitterArchiveImport.count", 1) do
      assert_enqueued_with(job: TwitterArchiveImportJob) do
        result = submission.submit
      end
    end

    assert_predicate result, :success?
    import = TwitterArchiveImport.last
    assert_equal "archive-bytes", File.binread(import.source_path)
    assert File.exist?(original_path), "expected fallback copy to leave the original tempfile in place"
  ensure
    uploaded_file&.tempfile&.close!
    TwitterArchiveImport.last&.cleanup_source_file!
  end

  test "submit rejects invalid uploads" do
    uploaded_file, = uploaded_zip_file(filename: "twitter-archive.txt", content_type: "text/plain")

    assert_no_difference("TwitterArchiveImport.count") do
      assert_no_enqueued_jobs only: TwitterArchiveImportJob do
        result = TwitterArchiveImportSubmission.new(uploaded_file).submit

        assert_not result.success?
        assert_equal TwitterArchiveImportSubmission::INVALID_UPLOAD_ALERT, result.alert
      end
    end
  ensure
    uploaded_file&.tempfile&.close!
  end

  test "submit copies archive bytes from a direct uploaded blob and queues an import" do
    archive_bytes = build_zip_bytes(
      "data/account.js" => "window.YTD.account.part0 = #{JSON.generate([ { account: { username: 'archive_owner' } } ])}"
    )
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new(archive_bytes),
      filename: "twitter-archive.zip",
      content_type: "application/zip"
    )
    result = nil

    assert_difference("TwitterArchiveImport.count", 1) do
      assert_enqueued_with(job: TwitterArchiveImportJob) do
        result = TwitterArchiveImportSubmission.new(twitter_archive_direct_upload_token(blob)).submit
      end
    end

    assert_predicate result, :success?
    assert_equal TwitterArchiveImportSubmission::SUCCESS_NOTICE, result.notice
    assert_equal "twitter-archive.zip", TwitterArchiveImport.last.source_filename
    assert ActiveStorage::Blob.exists?(blob.id)
  ensure
    TwitterArchiveImport.last&.cleanup_source_file!
    blob&.purge
  end

  test "submit rejects invalid direct upload blob ids" do
    assert_no_difference("TwitterArchiveImport.count") do
      assert_no_enqueued_jobs only: TwitterArchiveImportJob do
        result = TwitterArchiveImportSubmission.new("bad-signed-id").submit

        assert_not result.success?
        assert_equal TwitterArchiveImportSubmission::INVALID_UPLOAD_ALERT, result.alert
      end
    end
  end

  test "submit rejects non-zip direct uploaded blobs" do
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("plain-text"),
      filename: "notes.txt",
      content_type: "text/plain"
    )

    assert_no_difference("TwitterArchiveImport.count") do
      assert_no_enqueued_jobs only: TwitterArchiveImportJob do
        result = TwitterArchiveImportSubmission.new(twitter_archive_direct_upload_token(blob)).submit

        assert_not result.success?
        assert_equal TwitterArchiveImportSubmission::INVALID_UPLOAD_ALERT, result.alert
      end
    end

    assert_not ActiveStorage::Blob.exists?(blob.id)
  end

  test "submit rejects unscoped direct upload blob ids without purging the blob" do
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new(build_zip_bytes(
        "data/account.js" => "window.YTD.account.part0 = #{JSON.generate([ { account: { username: 'archive_owner' } } ])}"
      )),
      filename: "twitter-archive.zip",
      content_type: "application/zip"
    )

    assert_no_difference("TwitterArchiveImport.count") do
      assert_no_enqueued_jobs only: TwitterArchiveImportJob do
        result = TwitterArchiveImportSubmission.new(blob.signed_id).submit

        assert_not result.success?
        assert_equal TwitterArchiveImportSubmission::INVALID_UPLOAD_ALERT, result.alert
      end
    end

    assert ActiveStorage::Blob.exists?(blob.id)
  ensure
    blob&.purge
  end

  test "submit refuses to queue while another import is active" do
    create_active_import!
    uploaded_file, = uploaded_zip_file

    assert_no_difference("TwitterArchiveImport.count") do
      assert_no_enqueued_jobs only: TwitterArchiveImportJob do
        result = TwitterArchiveImportSubmission.new(uploaded_file).submit

        assert_not result.success?
        assert_equal TwitterArchiveImportSubmission::ACTIVE_IMPORT_ALERT, result.alert
      end
    end
  ensure
    uploaded_file&.tempfile&.close!
  end

  test "submit purges direct uploaded blobs when another import is active" do
    create_active_import!
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new(build_zip_bytes(
        "data/account.js" => "window.YTD.account.part0 = #{JSON.generate([ { account: { username: 'archive_owner' } } ])}"
      )),
      filename: "twitter-archive.zip",
      content_type: "application/zip"
    )

    assert_no_difference("TwitterArchiveImport.count") do
      assert_no_enqueued_jobs only: TwitterArchiveImportJob do
        result = TwitterArchiveImportSubmission.new(twitter_archive_direct_upload_token(blob)).submit

        assert_not result.success?
        assert_equal TwitterArchiveImportSubmission::ACTIVE_IMPORT_ALERT, result.alert
      end
    end

    assert_not ActiveStorage::Blob.exists?(blob.id)
  end

  test "submit reports the active import alert when import creation hits the unique active slot" do
    uploaded_file, = uploaded_zip_file

    TwitterArchiveImport.stub(:create_queued!, ->(*) { raise ActiveRecord::RecordNotUnique, "duplicate key" }) do
      assert_no_difference("TwitterArchiveImport.count") do
        assert_no_enqueued_jobs only: TwitterArchiveImportJob do
          result = TwitterArchiveImportSubmission.new(uploaded_file).submit

          assert_not result.success?
          assert_equal TwitterArchiveImportSubmission::ACTIVE_IMPORT_ALERT, result.alert
        end
      end
    end
  ensure
    uploaded_file&.tempfile&.close!
  end

  test "submit reports the active import alert when import creation fails active slot validation" do
    uploaded_file, = uploaded_zip_file

    invalid_import = TwitterArchiveImport.new
    invalid_import.errors.add(:active_slot, :taken)
    TwitterArchiveImport.stub(:create_queued!, ->(*) { raise ActiveRecord::RecordInvalid.new(invalid_import) }) do
      assert_no_difference("TwitterArchiveImport.count") do
        assert_no_enqueued_jobs only: TwitterArchiveImportJob do
          result = TwitterArchiveImportSubmission.new(uploaded_file).submit

          assert_not result.success?
          assert_equal TwitterArchiveImportSubmission::ACTIVE_IMPORT_ALERT, result.alert
        end
      end
    end
  ensure
    uploaded_file&.tempfile&.close!
  end

  test "submit fails the import and cleans up when queueing raises" do
    uploaded_file, = uploaded_zip_file
    original_perform_later = TwitterArchiveImportJob.method(:perform_later)
    TwitterArchiveImportJob.define_singleton_method(:perform_later) { |_id| raise "queue exploded" }

    result = nil

    assert_difference("TwitterArchiveImport.count", 1) do
      result = TwitterArchiveImportSubmission.new(uploaded_file).submit
    end

    assert_not result.success?
    assert_match "queue exploded", result.alert

    import = TwitterArchiveImport.order(:created_at).last
    assert_equal "failed", import.status
    assert_equal "queue exploded", import.error_message
    assert_nil import.source_path
  ensure
    TwitterArchiveImportJob.define_singleton_method(:perform_later, original_perform_later)
    uploaded_file&.tempfile&.close!
  end

  test "submit does not enqueue the job when queue logging fails" do
    uploaded_file, = uploaded_zip_file
    original_log = ActivityLog.method(:log!)
    ActivityLog.define_singleton_method(:log!) { |**_kwargs| raise "log exploded" }

    assert_no_enqueued_jobs only: TwitterArchiveImportJob do
      result = TwitterArchiveImportSubmission.new(uploaded_file).submit

      assert_not result.success?
      assert_match "log exploded", result.alert
    end

    import = TwitterArchiveImport.order(:created_at).last
    assert_equal "failed", import.status
    assert_equal "log exploded", import.error_message
    assert_nil import.source_path
  ensure
    ActivityLog.define_singleton_method(:log!, original_log)
    uploaded_file&.tempfile&.close!
  end

  private

  def uploaded_zip_file(contents: "archive-bytes", filename: "twitter-archive.zip", content_type: "application/zip")
    tempfile = Tempfile.new([ File.basename(filename, ".*"), File.extname(filename) ])
    tempfile.binmode
    tempfile.write(contents)
    tempfile.flush

    uploaded_file = ActionDispatch::Http::UploadedFile.new(
      tempfile: tempfile,
      filename: filename,
      type: content_type
    )

    [ uploaded_file, tempfile.path ]
  end

  def create_active_import!
    TwitterArchiveImport.create!(
      source_filename: "existing-twitter-archive.zip",
      status: "running",
      progress: 40,
      queued_at: Time.current
    )
  end

  def build_zip_bytes(files)
    Zip::OutputStream.write_buffer do |zip|
      files.each do |name, content|
        zip.put_next_entry(name)
        zip.write(content)
      end
    end.string
  end

  def twitter_archive_direct_upload_token(blob)
    blob.signed_id(purpose: :twitter_archive_import)
  end
end
