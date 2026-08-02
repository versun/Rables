# frozen_string_literal: true

require "test_helper"
require "open3"
require "base64"

class ShareControllerTest < ActiveSupport::TestCase
  # The Stimulus import cannot be resolved by bare node, so it is replaced
  # with a minimal Controller stub and the module is loaded via a data: URL.
  test "ignores repeat copy clicks while success feedback is showing" do
    source = File.read(Rails.root.join("app/javascript/controllers/share_controller.js"))
    source = source.sub('import { Controller } from "@hotwired/stimulus"', "class Controller {}")
    module_url = "data:text/javascript;base64,#{Base64.strict_encode64(source)}"

    script = <<~JS
      const timeouts = [];
      globalThis.setTimeout = (fn) => { timeouts.push(fn); return timeouts.length; };
      globalThis.document = { removeEventListener() {} };

      const { default: ShareController } = await import(#{module_url.inspect});
      const controller = new ShareController();
      controller.menuTarget = { style: {} };
      controller.boundCloseOnOutsideClick = () => {};

      const target = { innerHTML: "<i>copy</i>", style: {}, dataset: {} };

      controller.showCopySuccess(target);
      // Second click while the success state is visible must be ignored,
      // otherwise the already-copied HTML is captured as the "original".
      controller.showCopySuccess(target);

      timeouts.forEach(fn => fn());

      if (target.innerHTML !== "<i>copy</i>") {
        throw new Error("expected original HTML restored after repeated copy clicks, got: " + target.innerHTML);
      }
      if (target.dataset.copying) {
        throw new Error("expected copying flag to be cleared");
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
