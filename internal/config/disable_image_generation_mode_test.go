package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDisableImageGenerationMode_UnmarshalYAML(t *testing.T) {
	type wrapper struct {
		V DisableImageGenerationMode `yaml:"disable-image-generation"`
	}

	{
		var w wrapper
		if err := yaml.Unmarshal([]byte("disable-image-generation: false\n"), &w); err != nil {
			t.Fatalf("unmarshal false: %v", err)
		}
		if w.V != DisableImageGenerationOff {
			t.Fatalf("false => %v, want %v", w.V, DisableImageGenerationOff)
		}
	}

	{
		var w wrapper
		if err := yaml.Unmarshal([]byte("disable-image-generation: true\n"), &w); err != nil {
			t.Fatalf("unmarshal true: %v", err)
		}
		if w.V != DisableImageGenerationAll {
			t.Fatalf("true => %v, want %v", w.V, DisableImageGenerationAll)
		}
	}

	{
		var w wrapper
		if err := yaml.Unmarshal([]byte("disable-image-generation: chat\n"), &w); err != nil {
			t.Fatalf("unmarshal chat: %v", err)
		}
		if w.V != DisableImageGenerationChat {
			t.Fatalf("chat => %v, want %v", w.V, DisableImageGenerationChat)
		}
	}
}

func TestParseDisableImageGenerationStringEqualFold(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want DisableImageGenerationMode
	}{
		{name: "empty off", raw: "", want: DisableImageGenerationOff},
		{name: "mixed false", raw: " FaLsE ", want: DisableImageGenerationOff},
		{name: "no", raw: "NO", want: DisableImageGenerationOff},
		{name: "mixed true", raw: "\tTrUe\n", want: DisableImageGenerationAll},
		{name: "yes", raw: "YES", want: DisableImageGenerationAll},
		{name: "chat", raw: " ChAt ", want: DisableImageGenerationChat},
	}

	for i := range tests {
		got, err := parseDisableImageGenerationString(tests[i].raw)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tests[i].name, err)
		}
		if got != tests[i].want {
			t.Fatalf("%s: got %v, want %v", tests[i].name, got, tests[i].want)
		}
	}
}

func TestParseDisableImageGenerationStringInvalidErrorIsLowercase(t *testing.T) {
	_, err := parseDisableImageGenerationString(" BAD ")
	if err == nil {
		t.Fatal("expected invalid value error")
	}
	if !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("error = %q, want lowercase invalid value", err.Error())
	}
}

func BenchmarkParseDisableImageGenerationString(b *testing.B) {
	for b.Loop() {
		mode, err := parseDisableImageGenerationString(" ChAt ")
		if err != nil {
			b.Fatal(err)
		}
		if mode != DisableImageGenerationChat {
			b.Fatalf("mode = %v", mode)
		}
	}
}

func TestDisableImageGenerationMode_UnmarshalJSON(t *testing.T) {
	{
		var v DisableImageGenerationMode
		if err := json.Unmarshal([]byte("false"), &v); err != nil {
			t.Fatalf("unmarshal false: %v", err)
		}
		if v != DisableImageGenerationOff {
			t.Fatalf("false => %v, want %v", v, DisableImageGenerationOff)
		}
	}

	{
		var v DisableImageGenerationMode
		if err := json.Unmarshal([]byte("true"), &v); err != nil {
			t.Fatalf("unmarshal true: %v", err)
		}
		if v != DisableImageGenerationAll {
			t.Fatalf("true => %v, want %v", v, DisableImageGenerationAll)
		}
	}

	{
		var v DisableImageGenerationMode
		if err := json.Unmarshal([]byte(`"chat"`), &v); err != nil {
			t.Fatalf("unmarshal chat: %v", err)
		}
		if v != DisableImageGenerationChat {
			t.Fatalf("chat => %v, want %v", v, DisableImageGenerationChat)
		}
	}
}
