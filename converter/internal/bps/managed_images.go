package bps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime"
	"mime/multipart"
	"net/textproto"
)

// ImageUpload contains only an attachment payload, never account credentials.
type ImageUpload struct {
	Key         string
	ContentType string
	Body        []byte
}
type ManagedImages struct {
	images []inputImage
	ids    map[string]string
}

func imageKey(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func PrepareManagedImages(body map[string]any) (*ManagedImages, []ImageUpload, error) {
	images, err := collectImages(body)
	if err != nil {
		return nil, nil, err
	}
	plan := &ManagedImages{images: images, ids: map[string]string{}}
	uploads := []ImageUpload{}
	seen := map[string]bool{}
	for _, image := range images {
		key := imageKey(image.data)
		if seen[key] {
			continue
		}
		seen[key] = true
		ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[image.media]
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": "image" + ext}))
		h.Set("Content-Type", image.media)
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, nil, err
		}
		if _, err = part.Write(image.data); err != nil {
			return nil, nil, err
		}
		if err = w.Close(); err != nil {
			return nil, nil, err
		}
		uploads = append(uploads, ImageUpload{Key: key, ContentType: w.FormDataContentType(), Body: buf.Bytes()})
	}
	return plan, uploads, nil
}
func (p *ManagedImages) SetFile(key, id string) error {
	if !validText(id, 256) {
		return errors.New("invalid_attachment_response")
	}
	p.ids[key] = id
	return nil
}
func (p *ManagedImages) Apply() error {
	for _, image := range p.images {
		id := p.ids[imageKey(image.data)]
		if id == "" {
			return errors.New("missing_attachment_response")
		}
		delete(image.part, "image_url")
		image.part["file_id"] = id
		if image.part["detail"] == nil {
			image.part["detail"] = "auto"
		}
	}
	return nil
}
