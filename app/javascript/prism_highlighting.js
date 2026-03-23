function hasLanguageClass(element) {
  if (!element?.classList) return false;

  return Array.from(element.classList).some((className) => {
    return className.startsWith("language-");
  });
}

export function codeBlockHasLanguage(block) {
  if (hasLanguageClass(block)) return true;

  return hasLanguageClass(block?.closest?.("pre"));
}

export function configurePrism() {
  if (!window.Prism) return;

  window.Prism.manual = true;

  if (window.Prism.plugins?.autoloader) {
    window.Prism.plugins.autoloader.languages_path = "https://cdn.jsdelivr.net/npm/prismjs@1.29.0/components/";
  }
}

function resolveHighlightRoot(root) {
  if (root && typeof root.querySelectorAll === "function") return root;
  if (root?.target && typeof root.target.querySelectorAll === "function") return root.target;

  return document;
}

export function highlightAll(root = document) {
  if (!window.Prism) return;

  configurePrism();

  const highlightRoot = resolveHighlightRoot(root);

  highlightRoot.querySelectorAll("pre code").forEach((block) => {
    if (!codeBlockHasLanguage(block) && window.hljs && !block.classList.contains("hljs")) {
      window.hljs.highlightElement(block);
    }
  });

  window.Prism.highlightAllUnder(highlightRoot);
}
