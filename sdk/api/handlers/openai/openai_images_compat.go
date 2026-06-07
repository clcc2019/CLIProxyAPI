package openai

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

func normalizeImagesResponseFormat(responseFormat string) string {
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		return "url"
	}
	return "b64_json"
}

func mimeTypeFromOutputFormat(outputFormat string) string {
	if outputFormat == "" {
		return "image/png"
	}
	if strings.Contains(outputFormat, "/") {
		return outputFormat
	}
	outputFormat = strings.TrimSpace(outputFormat)
	switch {
	case strings.EqualFold(outputFormat, "png"):
		return "image/png"
	case strings.EqualFold(outputFormat, "jpg"):
		return "image/jpeg"
	case strings.EqualFold(outputFormat, "jpeg"):
		return "image/jpeg"
	case strings.EqualFold(outputFormat, "webp"):
		return "image/webp"
	default:
		return "image/png"
	}
}

func multipartFileToDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("upload file is nil")
	}
	f, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("open upload file failed: %w", err)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("openai images: close upload file error: %v", errClose)
		}
	}()

	data, err := util.ReadResponseBodyLimited(f, maxImageUploadBytes)
	if err != nil {
		if errors.Is(err, util.ErrResponseBodyTooLarge) {
			return "", fmt.Errorf("upload file exceeds maximum allowed size of %d bytes", maxImageUploadBytes)
		}
		return "", fmt.Errorf("read upload file failed: %w", err)
	}

	mediaType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	return "data:" + mediaType + ";base64," + b64, nil
}

func buildOpenAICompatImagesJSONRequest(rawJSON []byte, imageModel string, stream bool) []byte {
	payload := rawJSON
	if model := strings.TrimSpace(imageModel); model != "" {
		payload, _ = sjson.SetBytes(payload, "model", model)
	}
	if stream {
		payload, _ = sjson.SetBytes(payload, "stream", true)
	} else {
		payload, _ = sjson.DeleteBytes(payload, "stream")
	}
	return payload
}

func cloneMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func buildOpenAICompatImagesMultipartRequest(form *multipart.Form, imageModel string, stream bool) ([]byte, string, error) {
	if form == nil {
		return nil, "", fmt.Errorf("multipart form is nil")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if errWrite := writer.WriteField("model", imageModel); errWrite != nil {
		return nil, "", fmt.Errorf("write model field failed: %w", errWrite)
	}
	if stream {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return nil, "", fmt.Errorf("write stream field failed: %w", errWrite)
		}
	}
	for key, values := range form.Value {
		if key == "model" || key == "stream" {
			continue
		}
		for _, value := range values {
			if errWrite := writer.WriteField(key, value); errWrite != nil {
				return nil, "", fmt.Errorf("write form field %s failed: %w", key, errWrite)
			}
		}
	}

	for key, files := range form.File {
		for _, fileHeader := range files {
			if fileHeader == nil {
				continue
			}
			header := cloneMIMEHeader(fileHeader.Header)
			header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "application/octet-stream")
			}
			part, errCreate := writer.CreatePart(header)
			if errCreate != nil {
				return nil, "", fmt.Errorf("create file field %s failed: %w", key, errCreate)
			}
			src, errOpen := fileHeader.Open()
			if errOpen != nil {
				return nil, "", fmt.Errorf("open upload file failed: %w", errOpen)
			}
			_, errCopy := io.Copy(part, src)
			if errClose := src.Close(); errClose != nil {
				log.Errorf("openai images: close upload file error: %v", errClose)
				if errCopy == nil {
					errCopy = errClose
				}
			}
			if errCopy != nil {
				return nil, "", fmt.Errorf("copy upload file failed: %w", errCopy)
			}
		}
	}

	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}
