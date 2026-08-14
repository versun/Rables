// Minimal @rails/activestorage replacement for lexxy attachment uploads.
//
// lexxy uploads files with:
//   const { DirectUpload } = await import("@rails/activestorage")
//   const upload = new DirectUpload(file, url, delegate)
//   upload.delegate = realDelegate
//   upload.create((error, blob) => { ... })
// (the bare specifier resolves to this module through the import map in
// admin_layout.html). Real ActiveStorage does a two-step dance (create a blob
// record, then PUT the file to the storage service); the Go server instead
// has a single multipart endpoint, POST /admin/uploads, which responds 201
// with {"key": ..., "url": "/files/<key>"}. This shim keeps the DirectUpload
// interface lexxy drives and maps it onto that one request.
export class DirectUpload {
  constructor(file, url, delegate) {
    this.file = file;
    this.url = url;
    this.delegate = delegate;
  }

  create(callback) {
    const request = new XMLHttpRequest();
    request.open("POST", this.url);
    request.responseType = "json";

    // lexxy attaches its upload-progress listener and abort bookkeeping to
    // the request inside this delegate callback, so it must fire before
    // send().
    this.delegate?.directUploadWillStoreFileWithXHR?.(request);

    request.addEventListener("load", () => {
      const response = request.response;
      if (request.status >= 200 && request.status < 300 && response && response.key) {
        callback(null, this.#blob(response));
      } else {
        callback(new Error("Upload failed with status " + request.status));
      }
    });
    request.addEventListener("error", () => callback(new Error("Upload failed: network error")));
    request.addEventListener("abort", () => callback(new Error("Upload aborted")));

    const form = new FormData();
    form.append("file", this.file, this.file.name);
    request.send(form);
  }

  // The blob shape lexxy's attachment node expects after a successful upload.
  #blob(response) {
    const file = this.file;
    return {
      signed_id: response.key,
      attachable_sgid: null,
      filename: file.name,
      content_type: file.type,
      byte_size: file.size,
      // Same rule as lexxy's isPreviewableImage: SVG is excluded because the
      // server forces it to download (see serveFile's active-type handling).
      previewable: file.type.startsWith("image/") && !file.type.includes("svg"),
      preview_status_url: null,
      url: response.url,
    };
  }
}

export default { DirectUpload };
