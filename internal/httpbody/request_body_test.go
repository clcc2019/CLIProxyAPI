package httpbody

import (
	"bytes"
	"errors"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDecodeContentEncodedRequestBody(t *testing.T) {
	t.Run("identity chain returns raw body", func(t *testing.T) {
		raw := []byte(`{"model":"test"}`)
		decoded, err := DecodeContentEncodedRequestBody(raw, "Identity, , IDENTITY")
		if err != nil {
			t.Fatalf("DecodeContentEncodedRequestBody() error = %v", err)
		}
		if !bytes.Equal(decoded, raw) {
			t.Fatalf("decoded = %q, want %q", decoded, raw)
		}
	})

	t.Run("zstd with identity suffix", func(t *testing.T) {
		raw := []byte(`{"model":"compressed"}`)
		compressed := zstdEncodeForTest(t, raw)
		decoded, err := DecodeContentEncodedRequestBody(compressed, "ZsTd, Identity")
		if err != nil {
			t.Fatalf("DecodeContentEncodedRequestBody() error = %v", err)
		}
		if !bytes.Equal(decoded, raw) {
			t.Fatalf("decoded = %q, want %q", decoded, raw)
		}
	})

	t.Run("unsupported encoding", func(t *testing.T) {
		_, err := DecodeContentEncodedRequestBody([]byte(`{}`), "GZip")
		if err == nil {
			t.Fatal("expected unsupported encoding error")
		}
		if got, want := err.Error(), "unsupported request content encoding: gzip"; got != want {
			t.Fatalf("error = %q, want %q", got, want)
		}
	})

	t.Run("zstd decoded body limit", func(t *testing.T) {
		raw := []byte(`{"model":"compressed","input":"hello"}`)
		compressed := zstdEncodeForTest(t, raw)
		decoded, err := DecodeContentEncodedRequestBodyLimited(compressed, "zstd", int64(len(raw)))
		if err != nil {
			t.Fatalf("DecodeContentEncodedRequestBodyLimited() at limit error = %v", err)
		}
		if !bytes.Equal(decoded, raw) {
			t.Fatalf("decoded = %q, want %q", decoded, raw)
		}

		_, err = DecodeContentEncodedRequestBodyLimited(compressed, "zstd", int64(len(raw)-1))
		if !errors.Is(err, ErrDecodedRequestBodyTooLarge) {
			t.Fatalf("DecodeContentEncodedRequestBodyLimited() error = %v, want ErrDecodedRequestBodyTooLarge", err)
		}
	})
}

func BenchmarkDecodeContentEncodedRequestBodyIdentityChain(b *testing.B) {
	raw := []byte(`{"model":"test","stream":true}`)
	for i := 0; i < b.N; i++ {
		decoded, err := DecodeContentEncodedRequestBody(raw, "identity, identity, identity")
		if err != nil {
			b.Fatal(err)
		}
		if len(decoded) != len(raw) {
			b.Fatal("unexpected decoded body length")
		}
	}
}

func zstdEncodeForTest(t *testing.T, raw []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := encoder.Write(raw); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return compressed.Bytes()
}
