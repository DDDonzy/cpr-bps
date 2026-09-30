package bps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

type inputImage struct {
	part  map[string]any
	media string
	data  []byte
}

// 先验证整批图片再上传；不抓取任意外部 URL。
func collectImages(body map[string]any) ([]inputImage, error) {
	result := []inputImage{}
	items, _ := body["input"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		parts, _ := item["content"].([]any)
		for _, value := range parts {
			part, _ := value.(map[string]any)
			if part["type"] != "input_image" {
				continue
			}
			if item["role"] != "user" {
				return nil, errors.New("invalid_image_role")
			}
			imageURL, _ := part["image_url"].(string)
			fileID, _ := part["file_id"].(string)
			if fileID != "" && imageURL == "" {
				continue
			}
			if fileID != "" || !strings.HasPrefix(imageURL, "data:") {
				return nil, errors.New("unsupported_image_reference")
			}
			metadata, data, ok := strings.Cut(imageURL[5:], ",")
			if !ok || !strings.HasSuffix(metadata, ";base64") {
				return nil, errors.New("invalid_image")
			}
			media, _, err := mime.ParseMediaType(strings.TrimSuffix(metadata, ";base64"))
			if err != nil {
				return nil, errors.New("invalid_image")
			}
			switch media {
			case "image/png", "image/jpeg", "image/gif", "image/webp":
			default:
				return nil, errors.New("unsupported_image_type")
			}
			if len(data) > 12<<20 {
				return nil, errors.New("image_too_large")
			}
			decoded, err := base64.StdEncoding.Strict().DecodeString(data)
			if err != nil || len(decoded) == 0 || len(decoded) > 8<<20 || http.DetectContentType(decoded) != media {
				return nil, errors.New("invalid_image")
			}
			result = append(result, inputImage{part: part, media: media, data: decoded})
			if len(result) > 16 {
				return nil, errors.New("too_many_images")
			}
		}
	}
	return result, nil
}

func (b *Bridge) uploadImages(ctx context.Context, client *http.Client, e Envelope, images []inputImage) error {
	if len(images) == 0 {
		return nil
	}
	endpoint, err := url.Parse(b.responsesURL)
	if err != nil {
		return errors.New("invalid_attachment_endpoint")
	}
	endpoint = endpoint.ResolveReference(&url.URL{Path: "attachments"})
	// 缓存仅存活于当前请求，令牌、文件标识及正文不跨租户共享。
	uploaded := map[[32]byte]string{}
	for _, image := range images {
		key := sha256.Sum256(image.data)
		fileID := uploaded[key]
		if fileID == "" {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			// 上游按附件文件名判断可用的图片格式，不能丢掉已经验证的 MIME 扩展名。
			extensions := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}
			extension, ok := extensions[image.media]
			if !ok {
				return errors.New("unsupported_image_type")
			}
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": "image" + extension}))
			header.Set("Content-Type", image.media)
			part, err := writer.CreatePart(header)
			if err != nil {
				return errors.New("attachment_encoding_failed")
			}
			if _, err = part.Write(image.data); err != nil {
				return errors.New("attachment_encoding_failed")
			}
			if writer.Close() != nil {
				return errors.New("attachment_encoding_failed")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), io.NopCloser(bytes.NewReader(body.Bytes())))
			if err != nil {
				return errors.New("attachment_encoding_failed")
			}
			req.ContentLength = int64(body.Len())
			req.Header = authHeaders(e)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("Accept", "application/json")
			response, err := client.Do(req)
			if err != nil {
				return errors.New("attachment_transport_failed")
			}
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
			response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return errors.New("attachment_upload_failed")
			}
			var result struct {
				FileID string `json:"openai_file_id"`
			}
			if readErr != nil || len(raw) > 65536 || json.Unmarshal(raw, &result) != nil || !validText(result.FileID, 256) {
				return errors.New("invalid_attachment_response")
			}
			fileID = result.FileID
			uploaded[key] = fileID
		}
		delete(image.part, "image_url")
		image.part["file_id"] = fileID
		if image.part["detail"] == nil {
			image.part["detail"] = "auto"
		}
	}
	return nil
}
