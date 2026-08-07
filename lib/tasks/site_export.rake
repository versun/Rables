namespace :site do
  desc "Export the whole site (rendered HTML, media files, JSONL metadata) for the Go migration"
  task :export, [ :output_dir ] => :environment do |_task, args|
    output_dir = args[:output_dir] || Rails.root.join("tmp", "site_export_#{Time.current.strftime("%Y%m%d_%H%M%S")}")
    SiteExport.export(output_dir: output_dir)
  end
end
