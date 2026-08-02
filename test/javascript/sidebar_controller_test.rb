# frozen_string_literal: true

require "test_helper"
require "open3"
require "base64"

class SidebarControllerTest < ActiveSupport::TestCase
  # The Stimulus import cannot be resolved by bare node, so it is replaced
  # with a minimal Controller stub and the module is loaded via a data: URL.
  test "disconnect removes every listener registered in connect" do
    source = File.read(Rails.root.join("app/javascript/controllers/sidebar_controller.js"))
    source = source.sub('import { Controller } from "@hotwired/stimulus"', "class Controller {}")
    module_url = "data:text/javascript;base64,#{Base64.strict_encode64(source)}"

    script = <<~JS
      const added = [];
      const removed = [];
      const makeNode = () => ({
        addEventListener(type, handler) { added.push(handler); },
        removeEventListener(type, handler) { removed.push(handler); }
      });
      const overlay = makeNode();
      const sidebar = Object.assign(makeNode(), {
        querySelectorAll() { return [ makeNode(), makeNode() ]; },
        classList: { add() {}, remove() {}, contains() { return false; } }
      });

      const { default: SidebarController } = await import(#{module_url.inspect});
      const controller = new SidebarController();
      controller.hasOverlayTarget = true;
      controller.overlayTarget = overlay;
      controller.hasSidebarTarget = true;
      controller.sidebarTarget = sidebar;
      controller.hasToggleTarget = false;

      controller.connect();
      controller.disconnect();

      if (added.length === 0) {
        throw new Error("expected connect to register listeners");
      }
      if (removed.length !== added.length) {
        throw new Error("expected disconnect to remove all " + added.length + " listeners, removed " + removed.length);
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
