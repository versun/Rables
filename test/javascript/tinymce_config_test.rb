# frozen_string_literal: true

require "test_helper"
require "open3"

class TinymceConfigTest < ActiveSupport::TestCase
  test "enables code insertion tools for tinymce editors" do
    config = File.read(Rails.root.join("app/javascript/tinymce_config.js"))

    assert_includes config, "'codesample'"
    assert_includes config, "codesample_global_prismjs: true"
    assert_includes config, "codesample_languages"
    assert_includes config, "toolbar:"
    assert_includes config, "codesample"
    assert_includes config, "inlinecode"
  end

  test "skips initialization when tinymce is unavailable or no editor is present" do
    config = File.read(Rails.root.join("app/javascript/tinymce_config.js"))

    assert_includes config, "if (typeof tinymce === \"undefined\")"
    assert_includes config, "document.querySelector(\".tinymce-editor\")"
  end

  test "inline code styles exclude codesample blocks" do
    config = File.read(Rails.root.join("app/javascript/tinymce_config.js"))

    refute_match(/^\s+code:not\(\[class\*="language-"\]\)/, config)
    assert_includes config, ":not(pre) > code:not([class*=\"language-\"])"
  end

  test "initTinyMCE does not require formatter during setup" do
    module_url = "file://#{Rails.root.join('app/javascript/tinymce_config.js')}"

    script = <<~JS
      globalThis.document = {
        addEventListener() {},
        querySelector(selector) {
          return selector === ".tinymce-editor" ? {} : null;
        }
      };

      const addedButtons = [];

      globalThis.tinymce = {
        remove() {},
        init(options) {
          options.setup({
            ui: {
              registry: {
                addToggleButton(name) {
                  addedButtons.push(name);
                }
              }
            },
            on() {},
            execCommand() {},
            selection: { getNode() { return null; } },
            dom: { getParent() { return null; } }
          });
        }
      };

      const { initTinyMCE } = await import(#{module_url.inspect});
      initTinyMCE();

      if (!addedButtons.includes("inlinecode")) {
        throw new Error("expected inlinecode button registration");
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
