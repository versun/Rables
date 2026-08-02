namespace :active_storage do
  desc "Mirror files from primary storage (S3) to mirror storage (local)"
  task mirror: :environment do
    puts "Starting Active Storage mirror sync..."

    # 获取当前配置的存储服务
    current_service = ActiveStorage::Blob.service

    # 检查是否是 MirrorService
    unless current_service.is_a?(ActiveStorage::Service::MirrorService)
      puts "Error: Current Active Storage service is not a MirrorService."
      puts "Current service: #{current_service.class.name}"
      puts "Please configure your storage service to use a Mirror service (e.g., s3_mirror_to_local)."
      exit 1
    end

    # 获取主服务和镜像服务
    primary_service = current_service.primary
    mirror_services = current_service.mirrors

    if mirror_services.empty?
      puts "Error: No mirror services configured. Please check your storage.yml configuration."
      exit 1
    end

    # 使用第一个镜像服务
    mirror_service = mirror_services.first

    puts "Primary service: #{primary_service.class.name}"
    puts "Mirror service: #{mirror_service.class.name}"
    puts "-" * 50

    # 统计信息
    total_blobs = ActiveStorage::Blob.count
    synced_count = 0
    skipped_count = 0
    error_count = 0

    puts "Total blobs to process: #{total_blobs}"
    puts "-" * 50

    # 遍历所有 blob
    ActiveStorage::Blob.find_each.with_index do |blob, index|
      begin
        # 检查主存储中是否存在文件
        unless primary_service.exist?(blob.key)
          puts "[#{index + 1}/#{total_blobs}] Blob #{blob.id} (#{blob.filename}): Not found in primary storage, skipping..."
          skipped_count += 1
          next
        end

        # 检查镜像存储中是否已存在
        if mirror_service.exist?(blob.key)
          puts "[#{index + 1}/#{total_blobs}] Blob #{blob.id} (#{blob.filename}): Already exists in mirror, skipping..."
          skipped_count += 1
          next
        end

        # 从主存储下载文件
        puts "[#{index + 1}/#{total_blobs}] Blob #{blob.id} (#{blob.filename}): Downloading from primary storage..."

        # 使用 download 方法获取文件内容
        # download returns a String, but Disk service upload expects an IO
        io = StringIO.new(primary_service.download(blob.key))

        # 上传到镜像存储
        mirror_service.upload(blob.key, io, checksum: blob.checksum)

        synced_count += 1
        puts "  ✓ Successfully mirrored to local storage"

      rescue => e
        error_count += 1
        puts "  ✗ Error: #{e.message}"
        Rails.event.notify "active_storage.mirror.blob_failed",
          level: "error",
          component: "active_storage_mirror_rake",
          blob_id: blob.id,
          filename: blob.filename.to_s,
          error_message: e.message,
          error_class: e.class.name,
          backtrace: e.backtrace.join("\n")
      end
    end

    puts "-" * 50
    puts "Mirror sync completed!"
    puts "  Total: #{total_blobs}"
    puts "  Synced: #{synced_count}"
    puts "  Skipped: #{skipped_count}"
    puts "  Errors: #{error_count}"
  end
end
