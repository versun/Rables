# frozen_string_literal: true

require "test_helper"
require "open3"

class PrismHighlightingTest < ActiveSupport::TestCase
  test "treats prism language on parent pre as already specified" do
    module_url = "file://#{Rails.root.join('app/javascript/prism_highlighting.js')}"

    script = <<~JS
      import { codeBlockHasLanguage } from #{module_url.inspect};

      const block = {
        classList: [],
        closest(selector) {
          if (selector !== "pre") return null;

          return { classList: ["language-ruby"] };
        }
      };

      if (!codeBlockHasLanguage(block)) {
        throw new Error("expected parent pre language class to count");
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

  test "highlightAll falls back to highlightjs for unlabeled blocks" do
    module_url = "file://#{Rails.root.join('app/javascript/prism_highlighting.js')}"

    script = <<~JS
      const classes = [];
      const highlighted = [];
      const block = {
        classList: {
          add(name) {
            classes.push(name);
          },
          contains(name) {
            return classes.includes(name);
          },
          [Symbol.iterator]: function* () {
            yield* classes;
          }
        },
        closest() {
          return null;
        }
      };
      const documentStub = {
        querySelectorAll() {
          return [block];
        }
      };

      globalThis.document = documentStub;
      globalThis.window = {
        Prism: {
          plugins: {},
          highlightAllUnder() {}
        },
        hljs: {
          highlightElement(target) {
            highlighted.push(target);
          }
        }
      };

      const { highlightAll } = await import(#{module_url.inspect});
      highlightAll(documentStub);

      if (highlighted.length !== 1 || highlighted[0] !== block) {
        throw new Error("expected highlight.js fallback for unlabeled block");
      }

      if (classes.includes("language-none")) {
        throw new Error("did not expect Prism opt-out class on legacy block");
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

  test "highlightAll accepts DOM event objects" do
    module_url = "file://#{Rails.root.join('app/javascript/prism_highlighting.js')}"

    script = <<~JS
      const roots = [];
      const documentStub = {
        querySelectorAll() {
          return [];
        }
      };

      globalThis.document = documentStub;
      globalThis.window = {
        Prism: {
          plugins: {},
          highlightAllUnder(root) {
            roots.push(root);
          }
        }
      };

      const { highlightAll } = await import(#{module_url.inspect});
      highlightAll({ type: "turbo:load", target: documentStub });

      if (roots.length !== 1 || roots[0] !== documentStub) {
        throw new Error("expected DOM event input to fall back to document");
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
