# frozen_string_literal: true

require "fileutils"
require "pathname"
require "digest"
require "json"
require "open-uri"
require "uri"

# Full-site export for the Go rewrite migration.
#
# Renders every article and page to its final HTML (with every media URL
# rewritten to /images/<key>.<ext>), archives the untouched source markup
# next to it, downloads all ActiveStorage blobs (checksum-verified) plus any
# remote media referenced by the content, and dumps all metadata as JSONL.
#
# The output layout is the migration contract documented in
# docs/go-migration-prompt.md — keep the two in sync.
class SiteExport
  IMAGE_URL_ROOT = "/images"
  # Media references that get rewritten. <a> targets are rewritten only when
  # they point at a local ActiveStorage URL; remote link targets are never
  # downloaded. srcset holds a comma-separated list of URL + descriptor
  # candidates and is rewritten per candidate (see rewrite_srcset).
  MEDIA_REFERENCES = {
    "img" => %w[src srcset],
    "source" => %w[src srcset],
    "video" => %w[src poster],
    "audio" => %w[src],
    "a" => %w[href]
  }.freeze
  MEDIA_CONTENT_PREFIXES = %w[image/ video/ audio/].freeze
  # Hard cap on remote media downloads: the response body is read into memory
  # in one go, so an absurdly large (or hostile) response must not OOM the
  # job worker. Local blobs are unaffected — they stream through tempfiles.
  MAX_REMOTE_DOWNLOAD_BYTES = 100.megabytes
  # Host used only so absolute ActiveStorage URLs can be generated outside a
  # request cycle; every generated URL is rewritten to a local path during
  # export, so this value never survives into the output.
  RENDER_HOST = "export.invalid"

  def self.export(**options)
    new(**options).export
  end

  def initialize(output_dir:, logger: ->(message) { puts message }, max_remote_download_bytes: MAX_REMOTE_DOWNLOAD_BYTES)
    @output_dir = Pathname.new(output_dir)
    @logger = logger
    @max_remote_download_bytes = max_remote_download_bytes
    @warnings = []
    @url_map = {}
    @blob_files = {}
    @blob_rows = []
    @remote_files = {}
    @original_blob_cache = {}
  end

  def export
    prepare_directories
    export_blobs
    export_articles
    export_pages
    export_tags
    export_article_tags
    export_comments
    export_redirects
    export_subscribers
    write_jsonl("blobs.jsonl", @blob_rows)
    write_jsonl("url_map.jsonl", url_map_rows)
    print_summary
    @output_dir
  end

  private

  # --- Blobs ---------------------------------------------------------------

  # Downloads every blob except the ones backing ActiveStorage variants
  # (regenerable thumbnails). Referenced-but-missing blobs are also picked up
  # lazily during content rewriting via register_blob.
  def export_blobs
    variant_blob_ids = ActiveStorage::Attachment.where(record_type: "ActiveStorage::VariantRecord").distinct.pluck(:blob_id)
    scope = ActiveStorage::Blob.where.not(id: variant_blob_ids)

    log "Exporting #{scope.count} blobs..."
    scope.find_each { |blob| download_blob(blob) }
  end

  def download_blob(blob)
    return @blob_files[blob.key] if @blob_files.key?(blob.key)

    filename = "#{blob.key}#{extension_for(content_type: blob.content_type, filename: blob.filename.to_s)}"
    destination = images_dir.join(filename)

    # A previous run may have been interrupted mid-copy; never trust an
    # existing file that fails verification.
    if destination.exist? && !checksum_matches?(blob, destination)
      record_warning("Existing file for blob #{blob.key} (#{blob.filename}) failed checksum verification; downloading again")
      FileUtils.rm_f(destination)
    end

    unless destination.exist?
      blob.open do |tempfile|
        unless checksum_matches?(blob, tempfile.path)
          record_warning("Checksum mismatch for blob #{blob.key} (#{blob.filename}): expected #{blob.checksum}")
        end
        FileUtils.cp(tempfile.path, destination)
      end
    end

    @blob_files[blob.key] = "#{IMAGE_URL_ROOT}/#{filename}"
    @blob_rows << {
      key: blob.key,
      filename: blob.filename.to_s,
      content_type: blob.content_type,
      byte_size: blob.byte_size,
      checksum_base64_md5: blob.checksum,
      file: "images/#{filename}"
    }
    @blob_files[blob.key]
  rescue StandardError => e
    record_warning("Failed to download blob #{blob.key} (#{blob.filename}): #{e.message}")
    nil
  end

  def checksum_matches?(blob, path)
    return true if blob.checksum.blank?

    Digest::MD5.file(path).base64digest == blob.checksum
  end

  # --- Articles & pages ------------------------------------------------------

  def export_articles
    log "Exporting #{Article.count} articles..."
    write_jsonl("articles.jsonl", Article.find_each.map { |article| export_content_row("articles", article, article_fields(article)) })
  end

  def export_pages
    log "Exporting #{Page.count} pages..."
    write_jsonl("pages.jsonl", Page.find_each.map { |page| export_content_row("pages", page, page_fields(page)) })
  end

  # Writes the rendered HTML (media URLs rewritten) plus the untouched source,
  # and returns the shared JSONL row fields.
  def export_content_row(kind, record, fields)
    rendered = rewrite_media(render_record(record), context: "#{record.class.name}##{record.id}")
    basename = safe_filename(record.slug)
    fields.merge(
      content_file: write_text_file("#{kind}/#{basename}.html", rendered),
      source_file: write_text_file("#{kind}/#{basename}.source.html", source_html_for(record)),
      source_content_type: record.content_type
    )
  end

  def article_fields(article)
    {
      id: article.id,
      slug: article.slug,
      title: article.title,
      status: article.status,
      created_at: iso8601(article.created_at),
      updated_at: iso8601(article.updated_at),
      scheduled_at: iso8601(article.scheduled_at),
      description: article.description,
      excerpt: article.excerpt,
      meta_title: article.meta_title,
      meta_description: article.meta_description,
      meta_image: resolve_meta_image(article),
      source_url: article.source_url,
      source_author: article.source_author,
      comment: article.comment
    }
  end

  def page_fields(page)
    {
      id: page.id,
      slug: page.slug,
      title: page.title,
      status: page.status,
      created_at: iso8601(page.created_at),
      updated_at: iso8601(page.updated_at),
      scheduled_at: iso8601(page.scheduled_at),
      comment: page.comment,
      page_order: page.page_order,
      redirect_url: page.redirect_url
    }
  end

  # Rendering goes through the same partials the site itself uses; those
  # generate absolute URLs and therefore need a host outside a request cycle.
  # The host is replaced by local paths during rewriting and never survives.
  def render_record(record)
    previous = ActiveStorage::Current.url_options
    ActiveStorage::Current.url_options = { host: RENDER_HOST }
    record.rendered_content.to_s
  ensure
    ActiveStorage::Current.url_options = previous
  end

  def source_html_for(record)
    record.html? ? record.html_content.to_s : record.content.body.to_s
  end

  # --- Media URL rewriting ---------------------------------------------------

  def rewrite_media(html, context:)
    document = Nokogiri::HTML5.fragment(html.to_s)
    MEDIA_REFERENCES.each do |tag, attributes|
      attributes.each do |attribute|
        document.css("#{tag}[#{attribute}]").each do |node|
          url = node[attribute].to_s.strip
          next if url.blank?

          replacement = if attribute == "srcset"
            rewrite_srcset(url, context: context)
          else
            resolve_url(url, allow_remote_download: tag != "a", context: context)
          end
          node[attribute] = replacement if replacement
        end
      end
    end
    # Drop the ActionText wrapper tags (editor round-tripping cruft that the
    # site serves as unknown elements); their rendered children already carry
    # the final markup.
    document.css("action-text-attachment").each { |node| node.replace(node.children) }
    document.to_html
  end

  # A srcset value is a comma-separated list of "url [descriptor]" candidates.
  # Substituting each resolvable URL in place (instead of parsing and
  # rebuilding the list) keeps data: URIs and descriptors untouched.
  def rewrite_srcset(value, context:)
    rewritten = value
    # Longest first so a URL that is a prefix of another can't corrupt it.
    value.scan(/[^\s,]+/).uniq.sort_by { |url| -url.length }.each do |url|
      replacement = resolve_url(url, allow_remote_download: true, context: context)
      rewritten = rewritten.gsub(url, replacement) if replacement
    end
    rewritten
  end

  # Returns the local /images/... path for a URL, or nil to leave it untouched.
  def resolve_url(url, allow_remote_download:, context:)
    if (blob = ActiveStorageBlobFinder.find_blob_from_url(url))
      return register_blob(blob, old_url: url)
    end

    if url.include?("/rails/active_storage/")
      record_warning("Unresolved Active Storage URL in #{context}: #{url}")
      return nil
    end

    if url.start_with?("http://", "https://")
      # Public S3 services render direct bucket URLs; the object key is the
      # last path segment.
      if (blob = blob_from_public_url(url))
        return register_blob(blob, old_url: url)
      end
      return register_remote_image(url, context: context) if allow_remote_download
    end

    nil
  end

  def register_blob(blob, old_url:)
    path = download_blob(original_blob_for(blob))
    return nil unless path

    record_url_map(old_url, path)
    path
  end

  # Content usually displays a resized variant; the export always carries the
  # full-quality original instead.
  def original_blob_for(blob)
    @original_blob_cache.fetch(blob.id) do
      attachment = ActiveStorage::Attachment.find_by(blob_id: blob.id, record_type: "ActiveStorage::VariantRecord")
      variant_record = ActiveStorage::VariantRecord.find_by(id: attachment.record_id) if attachment
      @original_blob_cache[blob.id] = variant_record&.blob || blob
    end
  end

  def blob_from_public_url(url)
    key = URI.parse(url).path.to_s.split("/").last
    key.present? ? ActiveStorage::Blob.find_by(key: key) : nil
  rescue URI::InvalidURIError
    nil
  end

  # The Go side 301s legacy /rails/active_storage/* paths via this map.
  # External URLs (e.g. public S3) keep working on their own, so only local
  # ActiveStorage URLs are recorded.
  def record_url_map(old_url, new_path)
    return unless old_url.include?("/rails/active_storage/")

    old_path = URI.parse(old_url).path.presence || old_url
    @url_map[old_path] ||= new_path
  rescue URI::InvalidURIError
    @url_map[old_url] ||= new_path
  end

  def register_remote_image(url, context:)
    return @remote_files[url] if @remote_files.key?(url)

    @remote_files[url] = download_remote_image(url, context: context)
  end

  def download_remote_image(url, context:)
    key = "remote-#{Digest::MD5.hexdigest(url)}"
    io = URI.open(url, open_timeout: 10, read_timeout: 30)
    declared_length = io.respond_to?(:meta) ? io.meta["content-length"].to_i : 0
    if declared_length > @max_remote_download_bytes
      record_warning("Remote attachment in #{context} is too large (#{declared_length} bytes, limit #{@max_remote_download_bytes}): #{url}")
      return nil
    end

    data = io.read(@max_remote_download_bytes + 1)
    if data.bytesize > @max_remote_download_bytes
      record_warning("Remote attachment in #{context} exceeds the #{@max_remote_download_bytes} byte limit: #{url}")
      return nil
    end

    original_name = File.basename(URI.parse(url).path.to_s)
    content_type = media_content_type(io.content_type)

    # Object storage and CDNs often serve files as application/octet-stream
    # (or with no usable type at all); sniff the actual bytes before giving up.
    content_type ||= media_content_type(Marcel::MimeType.for(StringIO.new(data), name: original_name))

    unless content_type
      record_warning("Remote attachment in #{context} is not media (#{io.content_type.presence || "unknown"}): #{url}")
      return nil
    end

    filename = "#{key}#{extension_for(content_type: content_type, filename: original_name)}"
    File.binwrite(images_dir.join(filename), data)

    @blob_rows << {
      key: key,
      filename: original_name.presence || filename,
      content_type: content_type,
      byte_size: data.bytesize,
      checksum_base64_md5: Digest::MD5.base64digest(data),
      file: "images/#{filename}",
      remote_url: url
    }
    "#{IMAGE_URL_ROOT}/#{filename}"
  rescue StandardError => e
    record_warning("Failed to download remote image in #{context}: #{url} (#{e.message})")
    nil
  end

  def media_content_type(content_type)
    return if content_type.blank?

    content_type = content_type.split(";").first.to_s.strip
    MEDIA_CONTENT_PREFIXES.any? { |prefix| content_type.start_with?(prefix) } ? content_type : nil
  end

  def resolve_meta_image(article)
    value = article.meta_image
    return value if value.blank?

    resolve_url(value, allow_remote_download: true, context: "Article##{article.id} meta_image") || value
  end

  # --- Remaining tables ------------------------------------------------------

  def export_tags
    write_jsonl("tags.jsonl", Tag.find_each.map { |tag| { id: tag.id, name: tag.name, slug: tag.slug } })
  end

  def export_article_tags
    rows = ArticleTag.includes(:article, :tag).find_each.filter_map do |article_tag|
      unless article_tag.article && article_tag.tag
        record_warning("Skipping ArticleTag##{article_tag.id}: missing article or tag")
        next
      end

      { article_slug: article_tag.article.slug, tag_slug: article_tag.tag.slug }
    end
    write_jsonl("article_tags.jsonl", rows)
  end

  def export_comments
    rows = Comment.includes(:commentable, :article).find_each.filter_map do |comment|
      commentable = comment.commentable || comment.article
      unless commentable
        record_warning("Skipping Comment##{comment.id}: no commentable record")
        next
      end

      {
        id: comment.id,
        commentable_type: commentable.class.name,
        commentable_slug: commentable.slug,
        parent_id: comment.parent_id,
        author_name: comment.author_name,
        author_email: comment.author_email,
        author_url: comment.author_url,
        author_username: comment.author_username,
        author_avatar_url: comment.author_avatar_url,
        content: comment.content,
        status: comment.status,
        platform: comment.platform,
        external_id: comment.external_id,
        url: comment.url,
        published_at: iso8601(comment.published_at),
        created_at: iso8601(comment.created_at)
      }
    end
    write_jsonl("comments.jsonl", rows)
  end

  def export_redirects
    write_jsonl("redirects.jsonl", Redirect.find_each.map do |redirect|
      { regex: redirect.regex, replacement: redirect.replacement, enabled: redirect.enabled, permanent: redirect.permanent }
    end)
  end

  def export_subscribers
    write_jsonl("subscribers.jsonl", Subscriber.includes(:tags).find_each.map do |subscriber|
      {
        email: subscriber.email,
        confirmed_at: iso8601(subscriber.confirmed_at),
        unsubscribed_at: iso8601(subscriber.unsubscribed_at),
        confirmation_token: subscriber.confirmation_token,
        unsubscribe_token: subscriber.unsubscribe_token,
        created_at: iso8601(subscriber.created_at),
        tag_slugs: subscriber.tags.map(&:slug)
      }
    end)
  end

  # --- Helpers ----------------------------------------------------------------

  def prepare_directories
    FileUtils.mkdir_p(@output_dir.join("articles"))
    FileUtils.mkdir_p(@output_dir.join("pages"))
    FileUtils.mkdir_p(images_dir)
    FileUtils.mkdir_p(data_dir)
  end

  def images_dir = @output_dir.join("images")

  def data_dir = @output_dir.join("data")

  def write_text_file(relative_path, content)
    File.write(@output_dir.join(relative_path), content)
    relative_path
  end

  def write_jsonl(name, rows)
    File.open(data_dir.join(name), "w") do |file|
      rows.each { |row| file.puts(JSON.generate(row)) }
    end
  end

  def url_map_rows
    @url_map.sort.map { |old_path, new_path| { old_path: old_path, new_path: new_path } }
  end

  def extension_for(content_type:, filename: "")
    extension = MiniMime.lookup_by_content_type(content_type.to_s)&.extension
    extension = File.extname(filename).delete_prefix(".") if extension.blank?
    extension = extension.to_s.downcase
    extension = "jpg" if extension == "jpeg"
    extension.present? ? ".#{extension}" : ""
  end

  def safe_filename(slug)
    slug.to_s.gsub(%r{[/\\]}, "_")
  end

  def iso8601(time)
    time&.utc&.iso8601
  end

  def log(message)
    @logger.call(message)
  end

  def record_warning(message)
    @warnings << message
    log "  ! #{message}"
  end

  def print_summary
    log "Export complete: #{@output_dir}"
    log "  images: #{@blob_rows.size} (#{@remote_files.values.compact.size} remote)"
    log "  legacy URL mappings: #{@url_map.size}"
    return if @warnings.empty?

    log "  warnings (#{@warnings.size}):"
    @warnings.each { |message| log "    - #{message}" }
  end
end
