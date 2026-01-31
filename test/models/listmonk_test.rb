# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class ListmonkTest < ActiveSupport::TestCase
  test "campaign_body includes source reference before content when article has source" do
    listmonk = Listmonk.new
    article = articles(:source_article)

    body = listmonk.send(:campaign_body, article)

    assert_includes body, "Example Author"
    assert_includes body, "Example source quote."
    assert_includes body, "https://example.com/source"
    assert_includes body, "<p>Source article content</p>"
    assert body.index("Example source quote.") < body.index("Source article content")
  end

  test "campaign_body does not add reference when article has no source" do
    listmonk = Listmonk.new
    article = articles(:published_article)

    body = listmonk.send(:campaign_body, article)

    refute_includes body, "source-reference__quote"
    assert_includes body, "<p>Published article content</p>"
  end

  test "fetches lists/templates and sends newsletters through the api" do
    listmonk = Listmonk.create!(
      url: "https://listmonk.example.com",
      username: "user",
      api_key: "key",
      list_id: 1,
      template_id: 2
    )
    assert listmonk.configured?
    assert_not Listmonk.new.configured?

    lists_response = Net::HTTPOK.new("1.1", "200", "OK")
    lists_response.body = { data: { results: [ { "id" => 10 } ] } }.to_json
    lists_response.instance_variable_set(:@read, true)

    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &_block) { lists_response }) do
      assert_equal [ { "id" => 10 } ], listmonk.fetch_lists
    end

    templates_response = Net::HTTPBadRequest.new("1.1", "400", "Bad")
    templates_response.body = "{}"
    templates_response.instance_variable_set(:@read, true)
    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &_block) { templates_response }) do
      assert_equal [], listmonk.fetch_templates
    end

    create_response = Net::HTTPOK.new("1.1", "200", "OK")
    create_response.body = { data: { id: 123 } }.to_json
    create_response.instance_variable_set(:@read, true)
    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &_block) { create_response }) do
      assert_equal 123, listmonk.create_campaigns(articles(:published_article), "Site")
    end

    send_response = Net::HTTPOK.new("1.1", "200", "OK")
    send_response.body = "{}"
    send_response.instance_variable_set(:@read, true)
    listmonk.stub(:create_campaigns, 123) do
      Net::HTTP.stub(:start, ->(*_args, **_kwargs, &_block) { send_response }) do
        assert_equal true, listmonk.send_newsletter(articles(:published_article), "Site")
      end
    end

    listmonk.stub(:create_campaigns, nil) do
      assert_equal false, listmonk.send_newsletter(articles(:published_article), "Site")
    end

    bad_response = Net::HTTPBadRequest.new("1.1", "400", "Bad")
    bad_response.body = "{}"
    bad_response.instance_variable_set(:@read, true)
    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &_block) { bad_response }) do
      assert_nil listmonk.create_campaigns(articles(:published_article), "Site")
    end

    listmonk.stub(:create_campaigns, 123) do
      Net::HTTP.stub(:start, ->(*_args, **_kwargs, &_block) { bad_response }) do
        assert_nil listmonk.send_newsletter(articles(:published_article), "Site")
      end
    end
  end

  test "fetch helpers return empty when http raises" do
    listmonk = Listmonk.create!(
      url: "https://listmonk.example.com",
      username: "user",
      api_key: "key",
      list_id: 1,
      template_id: 2
    )

    Net::HTTP.stub(:start, ->(*_args, **_kwargs) { raise "boom" }) do
      assert_equal [], listmonk.fetch_lists
      assert_equal [], listmonk.fetch_templates
    end
  end

  test "fetch_templates returns data on success" do
    listmonk = Listmonk.create!(
      url: "https://listmonk.example.com",
      username: "user",
      api_key: "key",
      list_id: 1,
      template_id: 2
    )

    response = Net::HTTPOK.new("1.1", "200", "OK")
    response.body = { data: [ { "id" => 1 } ] }.to_json
    response.instance_variable_set(:@read, true)

    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &_block) { response }) do
      assert_equal [ { "id" => 1 } ], listmonk.fetch_templates
    end
  end

  test "uses http request blocks for listmonk calls" do
    listmonk = Listmonk.create!(
      url: "https://listmonk.example.com",
      username: "user",
      api_key: "key",
      list_id: 1,
      template_id: 2
    )

    lists_response = Net::HTTPOK.new("1.1", "200", "OK")
    lists_response.body = { data: { results: [] } }.to_json
    lists_response.instance_variable_set(:@read, true)
    http = Struct.new(:response) { def request(_req) response end }.new(lists_response)
    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &block) { block.call(http) }) do
      assert_equal [], listmonk.fetch_lists
    end

    templates_response = Net::HTTPOK.new("1.1", "200", "OK")
    templates_response.body = { data: [] }.to_json
    templates_response.instance_variable_set(:@read, true)
    http = Struct.new(:response) { def request(_req) response end }.new(templates_response)
    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &block) { block.call(http) }) do
      assert_equal [], listmonk.fetch_templates
    end

    create_response = Net::HTTPOK.new("1.1", "200", "OK")
    create_response.body = { data: { id: 9 } }.to_json
    create_response.instance_variable_set(:@read, true)
    http = Struct.new(:response) { def request(_req) response end }.new(create_response)
    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &block) { block.call(http) }) do
      assert_equal 9, listmonk.create_campaigns(articles(:published_article), "Site")
    end

    send_response = Net::HTTPOK.new("1.1", "200", "OK")
    send_response.body = "{}"
    send_response.instance_variable_set(:@read, true)
    http = Struct.new(:response) { def request(_req) response end }.new(send_response)
    listmonk.stub(:create_campaigns, 9) do
      Net::HTTP.stub(:start, ->(*_args, **_kwargs, &block) { block.call(http) }) do
        assert_equal true, listmonk.send_newsletter(articles(:published_article), "Site")
      end
    end
  end

  test "fetch_lists handles non-success responses" do
    listmonk = Listmonk.create!(
      url: "https://listmonk.example.com",
      username: "user",
      api_key: "key",
      list_id: 1,
      template_id: 2
    )

    bad_response = Net::HTTPBadRequest.new("1.1", "400", "Bad")
    bad_response.body = "{}"
    bad_response.instance_variable_set(:@read, true)
    http = Struct.new(:response) { def request(_req) response end }.new(bad_response)

    Net::HTTP.stub(:start, ->(*_args, **_kwargs, &block) { block.call(http) }) do
      assert_equal [], listmonk.fetch_lists
    end
  end
end
