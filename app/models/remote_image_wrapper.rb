# Wrapper class for remote image URLs extracted from HTML content
# This class provides a compatible interface with ActionText::Attachables::RemoteImage
# so that social media services can handle both types uniformly
class RemoteImageWrapper
  attr_reader :url

  def initialize(url)
    @url = url
  end
end
