// TinyMCE 8 Configuration for Rails Admin
// This configuration replaces Trix editor with TinyMCE

function initTinyMCE() {
  // Destroy existing instances to avoid conflicts
  if (typeof tinymce !== "undefined") {
    tinymce.remove();
  }

  // Initialize TinyMCE on all elements with class 'tinymce-editor'
  tinymce.init({
    selector: '.tinymce-editor',
    license_key: 'gpl',
    height: 500,
    menubar: false,
    plugins: [
      'advlist', 'autolink', 'lists', 'link', 'image', 'charmap', 'preview',
      'anchor', 'searchreplace', 'visualblocks', 'code', 'fullscreen',
      'insertdatetime', 'media', 'table', 'help', 'wordcount'
    ],
    toolbar: 'undo redo | blocks | bold italic | alignleft aligncenter alignright alignjustify | bullist numlist outdent indent | link image | code | removeformat | help',
    content_style: 'body { font-family: system-ui, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; font-size: 14px; line-height: 1.6; }',

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
