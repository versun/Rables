# frozen_string_literal: true

require "test_helper"
require "rake"
require "active_storage/service/mirror_service"

class ActiveStorageMirrorTaskTest < ActiveSupport::TestCase
  class FakeMirrorService
    attr_reader :primary, :mirrors

    def initialize(primary:, mirrors:)
      @primary = primary
      @mirrors = mirrors
    end

    def is_a?(klass)
      klass == ActiveStorage::Service::MirrorService || super
    end
  end

  class FakeStorage
    attr_reader :uploads

    def initialize(existing_keys: [], raise_on: [])
      @existing_keys = existing_keys
      @raise_on = raise_on
      @uploads = []
    end

    def exist?(key)
      @existing_keys.include?(key)
    end

    def download(key)
      "data-#{key}"
    end

    def upload(key, io, checksum:)
      raise "upload failed" if @raise_on.include?(key)

      @uploads << [ key, io, checksum ]
    end
  end

  FakeBlob = Struct.new(:id, :key, :filename, :checksum)

  setup do
    Rake.application = Rake::Application.new
    load Rails.root.join("lib/tasks/active_storage_mirror.rake")
    Rake::Task.define_task(:environment)
  end

  test "exits when storage is not a mirror service" do
    ActiveStorage::Blob.stub(:service, Object.new) do
      assert_raises(SystemExit) do
        with_task_reenabled("active_storage:mirror", &:invoke)
      end
    end
  end

  test "exits when no mirror services configured" do
    fake_mirror = FakeMirrorService.new(primary: FakeStorage.new, mirrors: [])

    ActiveStorage::Blob.stub(:service, fake_mirror) do
      assert_raises(SystemExit) do
        with_task_reenabled("active_storage:mirror", &:invoke)
      end
    end
  end

  test "mirrors blobs from primary to mirror" do
    primary = FakeStorage.new(existing_keys: [ "a", "b", "c" ])
    mirror = FakeStorage.new(existing_keys: [ "b" ], raise_on: [ "c" ])
    fake_mirror = FakeMirrorService.new(primary: primary, mirrors: [ mirror ])

    blobs = [
      FakeBlob.new(1, "a", "a.jpg", "checksum-a"),
      FakeBlob.new(2, "b", "b.jpg", "checksum-b"),
      FakeBlob.new(3, "c", "c.jpg", "checksum-c"),
      FakeBlob.new(4, "missing", "missing.jpg", "checksum-missing")
    ]

    ActiveStorage::Blob.stub(:service, fake_mirror) do
      ActiveStorage::Blob.stub(:count, blobs.length) do
        ActiveStorage::Blob.stub(:find_each, blobs.each) do
          with_task_reenabled("active_storage:mirror", &:invoke)
        end
      end
    end

    assert_equal 1, mirror.uploads.length
    assert_equal [ "a", "data-a", "checksum-a" ], mirror.uploads.first
  end

  private

  def with_task_reenabled(task_name)
    task = Rake::Task[task_name]
    task.reenable
    yield task
  end
end
