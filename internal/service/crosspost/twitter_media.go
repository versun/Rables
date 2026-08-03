package crosspost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// TwitterApi::MediaUploader constants.
const (
	twitterMaxImageSize   = 5 * 1024 * 1024  // MAX_IMAGE_SIZE (5.megabytes)
	twitterMaxGIFSize     = 15 * 1024 * 1024 // MAX_GIF_SIZE (15.megabytes)
	twitterImageChunkSize = 5 * 1024 * 1024  // IMAGE_CHUNK_SIZE_MB
	twitterGIFChunkSize   = 15 * 1024 * 1024 // GIF_CHUNK_SIZE_MB
)

// twitterContentTypeExtensions ports CONTENT_TYPE_EXTENSIONS.
var twitterContentTypeExtensions = map[string]string{
	"image/bmp":   "bmp",
	"image/gif":   "gif",
	"image/jpeg":  "jpg",
	"image/pjpeg": "jpg",
	"image/png":   "png",
	"image/tiff":  "tiff",
	"image/webp":  "webp",
}

// twitterExtensionMediaTypes is media_type_for_file for every extension
// extension_for_content_type can produce.
var twitterExtensionMediaTypes = map[string]string{
	"gif":  "image/gif",
	"jpg":  "image/jpeg",
	"png":  "image/png",
	"webp": "image/webp",
	"bmp":  "image/bmp",
	"tiff": "image/tiff",
}

// limitTwitterMediaAttachments ports limit_twitter_media_attachments: an
// animated GIF posts alone, replacing the whole image set.
func limitTwitterMediaAttachments(images []Image) []Image {
	for _, img := range images {
		if isTwitterGIF(img) {
			return []Image{img}
		}
	}
	return images
}

// isTwitterGIF ports animated_gif_attachable?: a blob matches on content
// type, a remote image on its URL extension. The collected Image carries
// both, so either signal marks the GIF.
func isTwitterGIF(img Image) bool {
	if normalizeTwitterContentType(img.ContentType) == "image/gif" {
		return true
	}
	return strings.EqualFold(filepath.Ext(img.Filename), ".gif")
}

// normalizeTwitterContentType ports normalize_content_type.
func normalizeTwitterContentType(contentType string) string {
	normalized := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if normalized == "" {
		return "image/jpeg"
	}
	return normalized
}

// uploadMedia ports TwitterApi::MediaUploader#upload + #upload_to_twitter:
// oversize images are recompressed (GIFs are dropped), then the bytes go
// through the chunked upload (INIT/APPEND/FINALIZE) and processing wait.
// Permanent failures return an error whose image is skipped by the caller
// (the Rails nil); transient failures abort the post.
func (p twitterPlatform) uploadMedia(ctx context.Context, client *http.Client, img Image) (string, error) {
	data := img.Data
	contentType := normalizeTwitterContentType(img.ContentType)

	maxSize := twitterMaxImageSize
	if contentType == "image/gif" {
		maxSize = twitterMaxGIFSize
	}
	if len(data) > maxSize {
		if contentType == "image/gif" { // gif_too_large → nil
			return "", fmt.Errorf("twitter: gif %q exceeds %d bytes", img.Filename, twitterMaxGIFSize)
		}
		compressed, ok := compressImage(data, twitterMaxImageSize)
		if !ok {
			return "", fmt.Errorf("twitter: compress %q: cannot fit %d bytes", img.Filename, twitterMaxImageSize)
		}
		data, contentType = compressed, "image/jpeg"
	}

	ext, ok := twitterContentTypeExtensions[contentType]
	if !ok {
		ext = "jpg"
	}
	mediaCategory := "tweet_image"
	if ext == "gif" {
		mediaCategory = "tweet_gif"
	}
	mediaType := twitterExtensionMediaTypes[ext]
	chunkSize := twitterImageChunkSize
	if mediaCategory == "tweet_gif" {
		chunkSize = twitterGIFChunkSize
	}

	mediaID, err := p.mediaUploadInit(ctx, client, mediaType, mediaCategory, len(data))
	if err != nil {
		return "", err
	}
	if mediaID == "" {
		return "", fmt.Errorf("twitter: media upload initialize returned no id")
	}
	for i, chunk := range splitChunks(data, chunkSize) {
		if err := p.mediaUploadAppend(ctx, client, mediaID, i, chunk); err != nil {
			return "", err
		}
	}
	media, err := p.mediaUploadFinalize(ctx, client, mediaID)
	if err != nil {
		return "", err
	}
	media, err = p.awaitProcessing(ctx, client, media)
	if err != nil {
		return "", err
	}
	if id := xDataID(map[string]any{"data": media}); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("twitter: media upload returned no id")
}

// mediaUploadInit ports X::MediaUploader.init: POST
// /2/media/upload/initialize with the JSON total_bytes/media_type/
// media_category triple.
func (p twitterPlatform) mediaUploadInit(ctx context.Context, client *http.Client, mediaType, mediaCategory string, totalBytes int) (string, error) {
	raw, err := json.Marshal(map[string]any{
		"media_type":     mediaType,
		"media_category": mediaCategory,
		"total_bytes":    totalBytes,
	})
	if err != nil {
		return "", err
	}
	body, err := p.doJSON(ctx, client, http.MethodPost, p.base()+"/media/upload/initialize", bytes.NewReader(raw), "application/json; charset=utf-8")
	if err != nil {
		return "", err
	}
	return xDataID(body), nil
}

// mediaUploadAppend ports X::MediaUploader.append's single-chunk request: one
// multipart POST /2/media/upload/<id>/append with the segment_index field and
// an octet-stream media part. Chunks go sequentially (the gem uses threads;
// the requests are identical). The gem's per-chunk retry maps onto the job
// retry via TransientError.
func (p twitterPlatform) mediaUploadAppend(ctx context.Context, client *http.Client, mediaID string, segmentIndex int, chunk []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("segment_index", fmt.Sprintf("%d", segmentIndex)); err != nil {
		return err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="media"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(chunk); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	_, err = p.doJSON(ctx, client, http.MethodPost, p.base()+"/media/upload/"+mediaID+"/append", &buf, mw.FormDataContentType())
	return err
}

// mediaUploadFinalize ports X::MediaUploader.chunked_upload's finalize POST.
func (p twitterPlatform) mediaUploadFinalize(ctx context.Context, client *http.Client, mediaID string) (map[string]any, error) {
	body, err := p.doJSON(ctx, client, http.MethodPost, p.base()+"/media/upload/"+mediaID+"/finalize", nil, "")
	if err != nil {
		return nil, err
	}
	media, _ := body["data"].(map[string]any)
	return media, nil
}

// awaitProcessing ports await_processing_if_needed + X::MediaUploader
// .await_processing!: poll STATUS while processing_info is pending; a failed
// state is a permanent upload error (the image is skipped, like the rescued
// "Media processing failed" RuntimeError returning nil).
func (p twitterPlatform) awaitProcessing(ctx context.Context, client *http.Client, media map[string]any) (map[string]any, error) {
	info, _ := media["processing_info"].(map[string]any)
	state, _ := info["state"].(string)
	if state == "" || state == "succeeded" {
		return media, nil
	}
	mediaID := xDataID(map[string]any{"data": media})
	for {
		u := p.base() + "/media/upload?command=STATUS&media_id=" + url.QueryEscape(mediaID)
		body, err := p.doJSON(ctx, client, http.MethodGet, u, nil, "")
		if err != nil {
			return nil, err
		}
		status, _ := body["data"].(map[string]any)
		info, _ := status["processing_info"].(map[string]any)
		state, _ := info["state"].(string)
		if status == nil || info == nil || state == "succeeded" {
			return status, nil
		}
		if state == "failed" {
			return nil, fmt.Errorf("twitter: Media processing failed")
		}
		wait := 0.0
		if secs, ok := info["check_after_secs"].(float64); ok {
			wait = secs
		}
		if err := p.wait(ctx, time.Duration(wait*float64(time.Second))); err != nil {
			return nil, err
		}
	}
}

// splitChunks ports X::MediaUploader.split: ceil(size/chunkSize) segments in
// order. Empty input yields no segments (APPEND is skipped entirely).
func splitChunks(data []byte, chunkSize int) [][]byte {
	var chunks [][]byte
	for start := 0; start < len(data); start += chunkSize {
		end := min(start+chunkSize, len(data))
		chunks = append(chunks, data[start:end])
	}
	return chunks
}
