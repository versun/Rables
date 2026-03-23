// TinyMCE 8 Configuration for Rails Admin
// This configuration replaces Trix editor with TinyMCE

const PRISM_THEME_URL = "https://cdn.jsdelivr.net/npm/prismjs@1.29.0/themes/prism.min.css";
const TINYMCE_CODE_LANGUAGES = [
  { text: "Plain text", value: "none" },
  { text: "Ruby", value: "ruby" },
  { text: "ERB", value: "erb" },
  { text: "JavaScript", value: "javascript" },
  { text: "HTML", value: "markup" },
  { text: "CSS", value: "css" },
  { text: "Bash", value: "bash" },
  { text: "JSON", value: "json" },
  { text: "YAML", value: "yaml" },
  { text: "SQL", value: "sql" },
  { text: "Markdown", value: "markdown" }
];

function setupInlineCodeButton(editor) {
  editor.ui.registry.addToggleButton("inlinecode", {
    text: "</>",
    tooltip: "Inline code",
    onAction: function () {
      editor.execCommand("mceToggleFormat", false, "inlinecode");
    },
    onSetup: function (api) {
      const toggleState = function () {
        const currentNode = editor.selection?.getNode?.();
        const inlineCodeNode = editor.dom?.getParent?.(currentNode, "code");
        api.setActive(Boolean(inlineCodeNode));
      };

      editor.on("NodeChange", toggleState);
      toggleState();

      return function () {
        editor.off?.("NodeChange", toggleState);
      };
    }
  });
}

function initTinyMCE() {
  if (typeof tinymce === "undefined") return;
  if (!document.querySelector(".tinymce-editor")) return;

  // Destroy existing instances to avoid conflicts
  tinymce.remove();

  // Initialize TinyMCE on all elements with class 'tinymce-editor'
  tinymce.init({
    selector: '.tinymce-editor',
    license_key: 'gpl',
    height: 500,
    menubar: false,
    plugins: [
      'advlist', 'autolink', 'lists', 'link', 'image', 'charmap', 'preview',
      'anchor', 'searchreplace', 'visualblocks', 'code', 'codesample', 'fullscreen',
      'insertdatetime', 'media', 'table', 'help', 'wordcount'
    ],
    toolbar: 'undo redo | blocks | bold italic inlinecode | alignleft aligncenter alignright alignjustify | bullist numlist outdent indent | link image codesample | code | removeformat | help',
    content_css: [PRISM_THEME_URL],
    formats: {
      inlinecode: {
        inline: "code",
        exact: true
      }
    },
    content_style: `
      body {
        font-family: system-ui, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
        font-size: 14px;
        line-height: 1.6;
      }

      :not(pre) > code:not([class*="language-"]) {
        font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace;
        font-size: 0.92em;
        background: #f6f8fa;
        border: 1px solid #d0d7de;
        border-radius: 4px;
        padding: 0.15em 0.35em;
      }

      pre[class*="language-"] {
        border: 1px solid #d0d7de;
        border-radius: 8px;
        margin: 1rem 0;
      }

      pre[class*="language-"] code {
        font-size: 13px;
      }
    `,
    codesample_global_prismjs: true,
    codesample_languages: TINYMCE_CODE_LANGUAGES,

    // URL handling - keep absolute URLs
    relative_urls: false,
    remove_script_host: false,
    convert_urls: false,

    // File upload configuration
    images_upload_url: '/admin/editor_images',
    images_upload_credentials: true,
    automatic_uploads: true,

    // Image upload handler for CSRF token
    images_upload_handler: function (blobInfo, progress) {
      return new Promise(function (resolve, reject) {
        var xhr = new XMLHttpRequest();
        xhr.open('POST', '/admin/editor_images');

        // Add CSRF token for Rails
        var csrfToken = document.querySelector('meta[name="csrf-token"]');
        if (csrfToken) {
          xhr.setRequestHeader('X-CSRF-Token', csrfToken.getAttribute('content'));
        }

        xhr.onload = function() {
          if (xhr.status === 200) {
            try {
              var json = JSON.parse(xhr.responseText);
              if (json && json.location) {
                resolve(json.location);
              } else {
                reject('Invalid JSON: ' + xhr.responseText);
              }
            } catch (e) {
              reject('Invalid JSON: ' + xhr.responseText);
            }
          } else {
            reject('HTTP Error: ' + xhr.status);
          }
        };

        xhr.onerror = function() {
          reject('Upload failed due to network error');
        };

        xhr.upload.onprogress = function(e) {
          progress(e.loaded / e.total * 100);
        };

        var formData = new FormData();
        formData.append('file', blobInfo.blob(), blobInfo.filename());
        xhr.send(formData);
      });
    },

    // File picker configuration for all supported file types
    file_picker_types: 'file image media',
    file_picker_callback: function (cb, value, meta) {
      var input = document.createElement('input');
      input.setAttribute('type', 'file');
      input.setAttribute('accept', meta.filetype === 'image' ? 'image/*' : '*/*');

      input.onchange = function () {
        var file = this.files[0];
        var formData = new FormData();
        formData.append('file', file);

        var xhr = new XMLHttpRequest();
        xhr.open('POST', '/admin/editor_images');

        var csrfToken = document.querySelector('meta[name="csrf-token"]');
        if (csrfToken) {
          xhr.setRequestHeader('X-CSRF-Token', csrfToken.getAttribute('content'));
        }

        xhr.onload = function() {
          if (xhr.status === 200) {
            try {
              var json = JSON.parse(xhr.responseText);
              if (json && json.location) {
                var options = { title: file.name };
                if (meta.filetype === 'file') {
                  options.text = file.name;
                }
                cb(json.location, options);
              } else {
                console.error('Invalid JSON:', xhr.responseText);
              }
            } catch (e) {
              console.error('Invalid JSON:', xhr.responseText);
            }
          } else {
            console.error('HTTP Error:', xhr.status);
          }
        };

        xhr.onerror = function() {
          console.error('Upload failed due to network error');
        };

        xhr.send(formData);
      };

      input.click();
    },

    // Setup hook for additional initialization
    setup: function (editor) {
      setupInlineCodeButton(editor);

      editor.on('init', function () {
        // Add custom class to editor container for styling
        editor.getContainer().classList.add('tinymce-rails-editor');
      });
    }
  });
}

// Initialize on DOMContentLoaded
document.addEventListener('DOMContentLoaded', initTinyMCE);

// Initialize on Turbo load (Rails 7+ with Turbo)
document.addEventListener('turbo:load', initTinyMCE);

// Cleanup before Turbo cache
document.addEventListener('turbo:before-cache', function() {
  if (typeof tinymce !== "undefined") {
    tinymce.remove();
  }
});

export { initTinyMCE };
