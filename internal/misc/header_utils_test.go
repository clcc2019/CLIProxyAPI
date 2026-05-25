package misc

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestBuildCodexUserAgent_MatchesUpstreamShape(t *testing.T) {
	got := BuildCodexUserAgent("1.2.3")
	pattern := regexp.MustCompile(`^codex_cli_rs/1\.2\.3 \([^;]+; [^)]+\) \S+$`)
	if !pattern.MatchString(got) {
		t.Fatalf("BuildCodexUserAgent(%q) = %q does not match expected shape", "1.2.3", got)
	}
	if _, err := http.NewRequest(http.MethodGet, "https://example.com", nil); err != nil {
		t.Fatalf("sanity request: %v", err)
	}
}

func TestBuildCodexUserAgent_FallsBackToDefaultVersion(t *testing.T) {
	got := BuildCodexUserAgent("  ")
	pattern := regexp.MustCompile(`^codex_cli_rs/` + regexp.QuoteMeta(CodexCLIVersion) + ` \(`)
	if !pattern.MatchString(got) {
		t.Fatalf("empty version should fall back to CodexCLIVersion, got %q", got)
	}
}

func TestCodexCLIVersionPinned(t *testing.T) {
	const want = "0.134.0-alpha.3"
	if CodexCLIVersion != want {
		t.Fatalf("CodexCLIVersion = %q, want %q", CodexCLIVersion, want)
	}
}

func TestCodexCLIVersionMeetsGpt55CatalogMinimum(t *testing.T) {
	minVersion := codexCatalogMinimalClientVersion(t, "gpt-5.5")

	if compareCodexSemver(t, CodexCLIVersion, minVersion) < 0 {
		t.Fatalf("CodexCLIVersion = %q, below gpt-5.5 minimal_client_version %q", CodexCLIVersion, minVersion)
	}
}

func TestBuildCodexUserAgent_IsValidHeaderValue(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("User-Agent", CodexCLIUserAgent)
	if got := req.Header.Get("User-Agent"); got != CodexCLIUserAgent {
		t.Fatalf("User-Agent roundtrip changed value: got %q, want %q", got, CodexCLIUserAgent)
	}
}

func TestResolveCodexOriginator_Precedence(t *testing.T) {
	t.Setenv(CodexOriginatorEnvVar, "")
	if got := ResolveCodexOriginator(""); got != CodexDefaultOriginator {
		t.Fatalf("default originator = %q, want %q", got, CodexDefaultOriginator)
	}
	t.Setenv(CodexOriginatorEnvVar, "codex_vscode")
	if got := ResolveCodexOriginator(""); got != "codex_vscode" {
		t.Fatalf("env originator = %q, want %q", got, "codex_vscode")
	}
	if got := ResolveCodexOriginator("  codex_atlas  "); got != "codex_atlas" {
		t.Fatalf("configured originator = %q, want %q", got, "codex_atlas")
	}
	t.Setenv(CodexOriginatorEnvVar, "")
	if got := ResolveCodexOriginator("bad\x01value"); got != CodexDefaultOriginator {
		t.Fatalf("invalid originator = %q, want fallback %q", got, CodexDefaultOriginator)
	}
}

func TestResolveCodexResidency_EmptyMeansSkip(t *testing.T) {
	t.Setenv(CodexResidencyEnvVar, "")
	if got := ResolveCodexResidency(""); got != "" {
		t.Fatalf("default residency must be empty, got %q", got)
	}
	t.Setenv(CodexResidencyEnvVar, "us-central")
	if got := ResolveCodexResidency(""); got != "us-central" {
		t.Fatalf("env residency = %q, want %q", got, "us-central")
	}
	if got := ResolveCodexResidency(" eu-west "); got != "eu-west" {
		t.Fatalf("configured residency = %q, want %q", got, "eu-west")
	}
}

func TestSanitizeTerminalToken_ReplacesControlChars(t *testing.T) {
	got := sanitizeTerminalToken("bad\ttoken with space\x01")
	expect := "bad_token_with_space_"
	if got != expect {
		t.Fatalf("sanitizeTerminalToken = %q, want %q", got, expect)
	}
}

func TestCodexTerminalFromEnvUsesTermProgramVersion(t *testing.T) {
	got := codexTerminalFromEnv(func(key string) string {
		switch key {
		case "TERM_PROGRAM":
			return "VTE"
		case "TERM_PROGRAM_VERSION":
			return "7600"
		case "TERM":
			return "xterm-256color"
		default:
			return ""
		}
	})

	if got != "VTE/7600" {
		t.Fatalf("codexTerminalFromEnv() = %q, want %q", got, "VTE/7600")
	}
}

func TestCodexTerminalFromEnvFallsBackToVTEVersion(t *testing.T) {
	got := codexTerminalFromEnv(func(key string) string {
		switch key {
		case "VTE_VERSION":
			return "7600"
		case "TERM":
			return "xterm-256color"
		default:
			return ""
		}
	})

	if got != "VTE/7600" {
		t.Fatalf("codexTerminalFromEnv() = %q, want %q", got, "VTE/7600")
	}
}

func TestCodexTerminalFromEnvSanitizesInvalidChars(t *testing.T) {
	got := codexTerminalFromEnv(func(key string) string {
		switch key {
		case "TERM_PROGRAM":
			return "bad term"
		case "TERM_PROGRAM_VERSION":
			return "1:2"
		default:
			return ""
		}
	})

	if got != "bad_term/1_2" {
		t.Fatalf("codexTerminalFromEnv() = %q, want %q", got, "bad_term/1_2")
	}
}

func TestCodexLinuxOSDescriptorPrefersNameAndVersionID(t *testing.T) {
	got := codexLinuxOSDescriptor(func(string) ([]byte, error) {
		return []byte("NAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.2 LTS\"\n"), nil
	})

	if got != "Ubuntu 24.04" {
		t.Fatalf("codexLinuxOSDescriptor() = %q, want %q", got, "Ubuntu 24.04")
	}
}

func TestCodexLinuxOSDescriptorFallsBackToPrettyName(t *testing.T) {
	got := codexLinuxOSDescriptor(func(string) ([]byte, error) {
		return []byte("PRETTY_NAME=\"Fedora Linux 41\"\n"), nil
	})

	if got != "Fedora Linux 41" {
		t.Fatalf("codexLinuxOSDescriptor() = %q, want %q", got, "Fedora Linux 41")
	}
}

func TestCodexDarwinProductVersion(t *testing.T) {
	got := codexDarwinProductVersion(func() ([]byte, error) {
		return []byte("14.6.1\n"), nil
	})

	if got != "14.6.1" {
		t.Fatalf("codexDarwinProductVersion() = %q, want 14.6.1", got)
	}
}

func TestCodexDarwinProductVersionRejectsUnexpectedOutput(t *testing.T) {
	got := codexDarwinProductVersion(func() ([]byte, error) {
		return []byte("bad version\n"), nil
	})

	if got != "" {
		t.Fatalf("codexDarwinProductVersion() = %q, want empty", got)
	}
}

func TestCodexCLIUserAgentWithOriginatorTrimsWhitespace(t *testing.T) {
	got := CodexCLIUserAgentWithOriginator("  codex_vscode  ")

	if !strings.HasPrefix(got, "codex_vscode/") {
		t.Fatalf("CodexCLIUserAgentWithOriginator() = %q, want codex_vscode/ prefix", got)
	}
}

func TestCodexCLIUserAgentWithOriginatorFallsBackToDefaultOriginator(t *testing.T) {
	got := CodexCLIUserAgentWithOriginator(" \t ")

	if got != CodexCLIUserAgent {
		t.Fatalf("CodexCLIUserAgentWithOriginator() = %q, want %q", got, CodexCLIUserAgent)
	}
}

func TestCodexCLIUserAgentWithOriginatorUsesNormalizedCacheKey(t *testing.T) {
	left := CodexCLIUserAgentWithOriginator("codex_vscode")
	right := CodexCLIUserAgentWithOriginator(" codex_vscode ")

	if left != right {
		t.Fatalf("normalized originators should produce identical user agents: %q != %q", left, right)
	}
}

func codexCatalogMinimalClientVersion(t *testing.T, slug string) string {
	t.Helper()

	type catalog struct {
		Models []struct {
			Slug                 string `json:"slug"`
			MinimalClientVersion string `json:"minimal_client_version"`
		} `json:"models"`
	}

	data, err := os.ReadFile(filepath.Join("..", "registry", "models", "codex_client_models.json"))
	if err != nil {
		t.Fatalf("read Codex model catalog: %v", err)
	}

	var parsed catalog
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse Codex model catalog: %v", err)
	}

	for _, model := range parsed.Models {
		if model.Slug == slug {
			if strings.TrimSpace(model.MinimalClientVersion) == "" {
				t.Fatalf("%s missing minimal_client_version in Codex model catalog", slug)
			}
			return model.MinimalClientVersion
		}
	}
	t.Fatalf("%s not found in Codex model catalog", slug)
	return ""
}

func compareCodexSemver(t *testing.T, left, right string) int {
	t.Helper()

	leftParts := parseCodexSemver(t, left)
	rightParts := parseCodexSemver(t, right)
	for i := range leftParts {
		if leftParts[i] > rightParts[i] {
			return 1
		}
		if leftParts[i] < rightParts[i] {
			return -1
		}
	}
	return 0
}

func parseCodexSemver(t *testing.T, version string) [3]int {
	t.Helper()

	core := strings.TrimSpace(version)
	if cut := strings.IndexAny(core, "-+"); cut >= 0 {
		core = core[:cut]
	}
	fields := strings.Split(core, ".")
	if len(fields) != 3 {
		t.Fatalf("invalid Codex semver %q", version)
	}

	var parsed [3]int
	for i, field := range fields {
		value, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("invalid Codex semver %q: %v", version, err)
		}
		parsed[i] = value
	}
	return parsed
}
