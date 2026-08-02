# Filter out harmless HTTP parse errors from Puma logs
# These errors occur when non-HTTP data is sent to the server (port scanners, etc.)
# and are usually harmless but add noise to logs
#
# To disable filtering, set FILTER_PUMA_PARSE_ERRORS=false

if Rails.env.development? && ENV.fetch("FILTER_PUMA_PARSE_ERRORS", "true") == "true"
  # Wrap STDERR to filter out HTTP parse errors
  # Puma writes these errors directly to STDERR, not through the logger
  original_stderr = $stderr

  filtered_stderr = Class.new do
    def initialize(original)
      @original = original
      @buffer = ""
    end

    def write(message)
      # Buffer multi-line messages
      @buffer << message.to_s

      # Check if the buffer contains HTTP parse errors
      if @buffer.include?("\n")
        lines = @buffer.split("\n", -1)
        @buffer = lines.pop || "" # Keep incomplete line in buffer

        lines.each do |line|
          write_line(line + "\n")
        end
      end
    end

    def flush
      flush_buffer
      @original.flush
    end

    def close
      # Flush any buffered partial line so it is not lost at shutdown
      flush_buffer
      @original.close
    end

    def method_missing(method, *args, &block)
      @original.send(method, *args, &block)
    end

    def respond_to_missing?(method, include_private = false)
      @original.respond_to?(method, include_private) || super
    end

    private

    def flush_buffer
      return if @buffer.empty?

      line = @buffer
      @buffer = ""
      write_line(line)
    end

    def write_line(line)
      # Filter out HTTP parse errors - they're usually from port scanners
      # or clients sending non-HTTP data to the HTTP port
      unless line.include?("HTTP parse error") ||
             line.include?("Invalid HTTP format") ||
             line.include?("malformed request") ||
             line.match?(/Bad method/)
        @original.write(line)
      end
    end
  end.new(original_stderr)

  $stderr = filtered_stderr
end
