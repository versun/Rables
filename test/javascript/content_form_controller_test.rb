# frozen_string_literal: true

require "test_helper"
require "open3"
require "base64"

class ContentFormControllerTest < ActiveSupport::TestCase
  # The Stimulus import cannot be resolved by bare node, so it is replaced
  # with a minimal Controller stub and the module is loaded via a data: URL.
  test "treats markup-only rich text content as blank" do
    source = File.read(Rails.root.join("app/javascript/controllers/content_form_controller.js"))
    source = source.sub('import { Controller } from "@hotwired/stimulus"', "class Controller {}")
    module_url = "data:text/javascript;base64,#{Base64.strict_encode64(source)}"

    script = <<~JS
      let editorContent = "<p>&nbsp;</p>";
      globalThis.tinymce = {
        triggerSave() {},
        get() { return { getContent: () => editorContent }; }
      };
      globalThis.alert = () => {};

      const { default: ContentFormController } = await import(#{module_url.inspect});
      const controller = new ContentFormController();
      controller.element = { querySelector() { return null; } };
      controller.paramValue = "article";
      controller.contentTypeSelectTarget = { value: "rich_text" };

      let prevented = 0;
      controller.submit({ preventDefault() { prevented += 1; } });
      if (prevented !== 1) {
        throw new Error("expected markup-only rich text content to block submit");
      }

      editorContent = "<p>Hello world</p>";
      controller.submit({ preventDefault() { throw new Error("should not block non-empty content"); } });
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
