package misc

import (
	"strings"
	"testing"
)

func TestAntigravityUserAgentMatchesHubFingerprint(t *testing.T) {
	ua := AntigravityUserAgent()
	if !strings.HasPrefix(ua, "antigravity/hub/") {
		t.Fatalf("AntigravityUserAgent() = %q, want Hub prefix", ua)
	}
	if strings.Contains(ua, "google-api-nodejs-client/") {
		t.Fatalf("AntigravityUserAgent() = %q, want short UA", ua)
	}
}

func TestAntigravityControlPlaneUserAgentSplit(t *testing.T) {
	short := AntigravityLoadCodeAssistUserAgent("")
	long := AntigravityOnboardUserUserAgent("")
	if strings.Contains(short, "google-api-nodejs-client/") {
		t.Fatalf("loadCodeAssist UA = %q, want short UA", short)
	}
	if !strings.Contains(long, "google-api-nodejs-client/10.3.0") {
		t.Fatalf("onboardUser UA = %q, want node client suffix", long)
	}
}

func TestAntigravityVersionFromHubUserAgent(t *testing.T) {
	if got := AntigravityVersionFromUserAgent("antigravity/hub/2.9.1 darwin/arm64"); got != "2.9.1" {
		t.Fatalf("AntigravityVersionFromUserAgent() = %q, want 2.9.1", got)
	}
}
