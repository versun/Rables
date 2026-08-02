# frozen_string_literal: true

require "test_helper"
require "open3"
require "base64"

class NewsletterControllerJsTest < ActiveSupport::TestCase
  # The Stimulus import cannot be resolved by bare node, so it is replaced
  # with a minimal Controller stub and the module is loaded via a data: URL.
  test "verify sends current selection and keeps current list/template selected" do
    source = File.read(Rails.root.join("app/javascript/controllers/newsletter_controller.js"))
    source = source.sub('import { Controller } from "@hotwired/stimulus"', "class Controller {}")
    module_url = "data:text/javascript;base64,#{Base64.strict_encode64(source)}"

    script = <<~JS
      const createdOptions = [];
      const elements = {
        listmonk_url: { value: "https://listmonk.test" },
        listmonk_username: { value: "user" },
        listmonk_api_key: { value: "key" }
      };
      const makeSelect = (value) => ({
        value,
        innerHTML: "",
        children: [],
        appendChild(option) { this.children.push(option); }
      });
      const listSelect = makeSelect("3");
      const templateSelect = makeSelect("7");

      globalThis.document = {
        getElementById(id) { return elements[id]; },
        querySelector() { return { content: "csrf-token" }; },
        createElement() {
          const option = { value: "", textContent: "", selected: false };
          createdOptions.push(option);
          return option;
        }
      };

      const requests = [];
      globalThis.fetch = async (url, options) => {
        requests.push(JSON.parse(options.body));
        return {
          json: async () => ({
            success: true,
            // API ids are numeric while current_*_id comes back as form strings
            lists: [ { id: 3, name: "News" } ],
            templates: [ { id: 7, name: "Default" } ],
            current_list_id: "3",
            current_template_id: "7"
          })
        };
      };

      const { default: NewsletterController } = await import(#{module_url.inspect});
      const controller = new NewsletterController();
      controller.verifyUrlValue = "/admin/newsletter/verify";
      controller.verifyBtnTarget = { disabled: false };
      controller.verifyStatusTarget = { textContent: "", style: {} };
      controller.listSelectTarget = listSelect;
      controller.templateSelectTarget = templateSelect;

      await controller.verify();

      const body = requests[0] || {};
      if (body.list_id !== "3" || body.template_id !== "7") {
        throw new Error("expected verify request to include current list_id/template_id");
      }

      if (!createdOptions.find(o => o.textContent === "News")?.selected) {
        throw new Error("expected current list option to stay selected");
      }
      if (!createdOptions.find(o => o.textContent === "Default")?.selected) {
        throw new Error("expected current template option to stay selected");
      }
    JS

    stdout, stderr, status = Open3.capture3("node", "--input-type=module", "--eval", script)

    assert status.success?, <<~MSG
      node assertion failed
      stdout:
      #{stdout}
      stderr:
      #{stderr}
    MSG
  end
end
