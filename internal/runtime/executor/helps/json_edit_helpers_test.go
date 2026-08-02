package helps

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEditJSONBytesAppliesOrderedMutations(t *testing.T) {
	body := []byte(`{"model":"old","stream":false,"previous_response_id":"resp_1","prompt_cache_retention":"keep"}`)

	got := EditJSONBytes(body,
		SetJSONEdit("model", "gpt-5"),
		SetJSONEdit("stream", true),
		DeleteJSONEdit("previous_response_id"),
		DeleteJSONEdit("prompt_cache_retention"),
	)

	if model := gjson.GetBytes(got, "model").String(); model != "gpt-5" {
		t.Fatalf("model = %q, want %q", model, "gpt-5")
	}
	if stream := gjson.GetBytes(got, "stream").Bool(); !stream {
		t.Fatal("stream = false, want true")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id still exists in %s", string(got))
	}
	if gjson.GetBytes(got, "prompt_cache_retention").Exists() {
		t.Fatalf("prompt_cache_retention still exists in %s", string(got))
	}
}

func TestDeleteJSONBytesSkipsMissingPath(t *testing.T) {
	body := []byte(`{"model":"gpt-5"}`)

	got, err := DeleteJSONBytes(body, "stream_options")
	if err != nil {
		t.Fatalf("DeleteJSONBytes error: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("DeleteJSONBytes changed body on missing path\n got: %s\nwant: %s", string(got), string(body))
	}
}

func TestSetStringIfDifferentReusesCanonicalValue(t *testing.T) {
	input := []byte(`{"model":"gpt-test","messages":[]}`)
	output := SetStringIfDifferent(input, "model", "gpt-test")
	if &output[0] != &input[0] {
		t.Fatal("canonical string caused a payload copy")
	}
}

func TestSetBoolIfDifferentReusesCanonicalValue(t *testing.T) {
	input := []byte(`{"stream":true}`)
	output := SetBoolIfDifferent(input, "stream", true)
	if &output[0] != &input[0] {
		t.Fatal("canonical boolean caused a payload copy")
	}
}

func TestSetRawIfDifferentReusesIdenticalValue(t *testing.T) {
	input := []byte(`{"metadata":{"source":"executor"}}`)
	output := SetRawIfDifferent(input, "metadata", []byte(`{"source":"executor"}`))
	if &output[0] != &input[0] {
		t.Fatal("identical raw value caused a payload copy")
	}
}

func TestSetIfDifferentNormalizesChangedValues(t *testing.T) {
	input := []byte(`{"model":123,"stream":"false","metadata":"old"}`)
	original := bytes.Clone(input)
	output := SetStringIfDifferent(input, "model", "gpt-test")
	output = SetBoolIfDifferent(output, "stream", true)
	output = SetRawIfDifferent(output, "metadata", []byte(`{"source":"executor"}`))
	if string(output) == string(input) {
		t.Fatal("changed values were not updated")
	}
	if !bytes.Equal(input, original) {
		t.Fatal("input payload was modified while normalizing values")
	}
}
