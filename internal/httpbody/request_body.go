package httpbody

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

const DefaultDecodedRequestBodyLimit = util.DefaultResponseBodyLimit

var ErrDecodedRequestBodyTooLarge = errors.New("decoded request body too large")

// DecodeContentEncodedRequestBody decodes request bodies according to
// Content-Encoding values. Encodings are applied in reverse header order.
func DecodeContentEncodedRequestBody(raw []byte, encoding string) ([]byte, error) {
	return DecodeContentEncodedRequestBodyLimited(raw, encoding, DefaultDecodedRequestBodyLimit)
}

// DecodeContentEncodedRequestBodyLimited decodes request bodies according to
// Content-Encoding values and caps the decoded representation size.
func DecodeContentEncodedRequestBodyLimited(raw []byte, encoding string, maxDecodedBytes int64) ([]byte, error) {
	body := raw
	encoding = strings.TrimSpace(encoding)
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return body, nil
	}
	if maxDecodedBytes <= 0 {
		maxDecodedBytes = DefaultDecodedRequestBodyLimit
	}

	for remaining := encoding; ; {
		idx := strings.LastIndexByte(remaining, ',')
		part := remaining
		if idx >= 0 {
			part = remaining[idx+1:]
			remaining = remaining[:idx]
		}

		enc := strings.TrimSpace(part)
		switch {
		case enc == "" || strings.EqualFold(enc, "identity"):
			// no-op
		case strings.EqualFold(enc, "zstd"):
			decoded, err := decodeZstdRequestBody(body, maxDecodedBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", strings.ToLower(enc))
		}

		if idx < 0 {
			break
		}
	}
	return body, nil
}

func decodeZstdRequestBody(raw []byte, maxDecodedBytes int64) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	decoded, err := readDecodedRequestBodyLimited(decoder, maxDecodedBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	return decoded, nil
}

func readDecodedRequestBodyLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = DefaultDecodedRequestBodyLimit
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(decoded)) > maxBytes {
		return nil, fmt.Errorf("%w: limit=%d", ErrDecodedRequestBodyTooLarge, maxBytes)
	}
	return decoded, nil
}
