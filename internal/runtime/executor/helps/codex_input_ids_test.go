package helps

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeCodexInputItemIDsBoundaries(t *testing.T) {
	id64 := strings.Repeat("a", 64)
	id65 := strings.Repeat("b", 65)
	unicode65 := strings.Repeat("界", 65)
	body := []byte(`{"input":[{"id":"` + id64 + `"},{"id":"` + id65 + `"},{"id":"` + unicode65 + `"}]}`)

	got := SanitizeCodexInputItemIDs(body)
	if actual := gjson.GetBytes(got, "input.0.id").String(); actual != id64 {
		t.Fatalf("64-character ID changed: %q", actual)
	}
	for _, path := range []string{"input.1.id", "input.2.id"} {
		actual := gjson.GetBytes(got, path).String()
		if len([]rune(actual)) != 64 {
			t.Fatalf("%s length = %d, want 64: %q", path, len([]rune(actual)), actual)
		}
	}
}

func TestSanitizeCodexInputItemIDsDropsOverlongEncryptedReasoningItem(t *testing.T) {
	longReasoningID := "rs_" + strings.Repeat("a", 64)
	shortReasoningID := "rs_" + strings.Repeat("b", 48)
	longCallID := strings.Repeat("call-item-", 8)
	body := []byte(`{"input":[` +
		`{"type":"message","id":"msg-1","role":"user","content":"before"},` +
		`{"type":"reasoning","id":"` + longReasoningID + `","encrypted_content":"gAAAA-encrypted","summary":[{"type":"summary_text","text":"drop me"}]},` +
		`{"type":"reasoning","id":"` + shortReasoningID + `","encrypted_content":"gAAAA-encrypted","summary":[]},` +
		`{"type":"function_call","id":"` + longCallID + `","call_id":"call-1","name":"lookup","arguments":"{}"}` +
		`]}`)

	got := SanitizeCodexInputItemIDs(body)
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 3 {
		t.Fatalf("input length = %d, want 3: %s", len(input), got)
	}
	if gotID := input[1].Get("id").String(); gotID != shortReasoningID {
		t.Fatalf("short encrypted reasoning id changed: %q", gotID)
	}
	if gotID := input[2].Get("id").String(); gotID == longCallID || len([]rune(gotID)) != 64 {
		t.Fatalf("ordinary overlong id was not shortened: %q", gotID)
	}
}

func TestSanitizeCodexInputItemIDsShortensOverlongReasoningWithoutEncryptedContent(t *testing.T) {
	longID := "rs_" + strings.Repeat("a", 64)
	for _, suffix := range []string{"", `,"encrypted_content":""`, `,"encrypted_content":null`} {
		body := []byte(`{"input":[{"type":"reasoning","id":"` + longID + `"` + suffix + `,"summary":[]}]}`)
		gotID := gjson.GetBytes(SanitizeCodexInputItemIDs(body), "input.0.id").String()
		if gotID == longID || len([]rune(gotID)) != 64 {
			t.Fatalf("overlong reasoning id was not shortened: %q", gotID)
		}
	}
}

func TestSanitizeCodexInputItemIDsAvoidsExistingIDCollision(t *testing.T) {
	longID := strings.Repeat("grok-item-", 10)
	collision := shortenCodexInputItemID(longID)
	body := []byte(`{"input":[{"id":"` + longID + `"},{"id":"` + collision + `"}]}`)
	first := SanitizeCodexInputItemIDs(body)
	second := SanitizeCodexInputItemIDs(body)
	shortened := gjson.GetBytes(first, "input.0.id").String()
	if shortened == collision || shortened != gjson.GetBytes(second, "input.0.id").String() {
		t.Fatalf("collision resolution is invalid or non-deterministic: %q", shortened)
	}
}

func TestSanitizeCodexInputItemIDsLeavesUnsupportedPayloadsUnchanged(t *testing.T) {
	for _, body := range [][]byte{[]byte(`not-json`), []byte(`{"input":{"id":"item-1"}}`), []byte(`{"input":[1,{"id":2},{"id":"item-1"}]}`)} {
		if got := string(SanitizeCodexInputItemIDs(body)); got != string(body) {
			t.Fatalf("payload changed: got=%q want=%q", got, body)
		}
	}
}

func benchInputItemsBody(items int, longIDs bool) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5","input":[`)
	for i := 0; i < items; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		id := fmt.Sprintf("msg_%040d", i)
		if longIDs {
			id = "msg_" + strings.Repeat("a", 90) + strconv.Itoa(i)
		}
		fmt.Fprintf(&b, `{"type":"message","id":%q,"role":"user","content":[{"type":"input_text","text":"%s"}]}`,
			id, strings.Repeat("hello world ", 40))
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// The overwhelmingly common case: every ID is within the limit, so the whole
// call should be a no-op that neither rebuilds nor reallocates the payload.
func BenchmarkSanitizeCodexInputItemIDsWithinLimit(b *testing.B) {
	body := benchInputItemsBody(200, false)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SanitizeCodexInputItemIDs(body)
	}
}

func BenchmarkSanitizeCodexInputItemIDsOverlong(b *testing.B) {
	body := benchInputItemsBody(200, true)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SanitizeCodexInputItemIDs(body)
	}
}
