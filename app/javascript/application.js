// Configure your import map in config/importmap.rb. Read more: https://github.com/rails/importmap-rails
import "@hotwired/turbo-rails";
import * as ActiveStorage from "@rails/activestorage";
import "controllers";
import "@rails/actiontext";
import hljs from "highlight.js";
import { configurePrism, highlightAll } from "prism_highlighting";
import "tinymce_config";

ActiveStorage.start();

// The +esm build of highlight.js only exposes module exports, so assign the
// global explicitly to enable the no-language-class fallback in prism_highlighting.
window.hljs = hljs;

// turbo:load also fires on the initial page load, so a separate
// DOMContentLoaded listener would highlight the first page twice.
document.addEventListener("turbo:load", highlightAll);

configurePrism();
