//go:build has_tui

package tui

import (
	"strings"
	"testing"
	"time"
)

func TestOAuthTabUsesDeviceCodeModeWhenCodeIsReturned(t *testing.T) {
	originalLocale := CurrentLocale()
	SetLocale("en")
	t.Cleanup(func() { SetLocale(originalLocale) })

	model := newOAuthTabModel(nil)
	model.SetSize(100, 30)
	updated, cmd := model.Update(oauthStartMsg{
		url:          "https://auth.openai.com/codex/device",
		state:        "device-state",
		providerName: "Codex (OpenAI)",
		deviceCode:   "ABCD-1234",
		timeout:      time.Minute,
	})

	if updated.state != oauthDevice || updated.inputActive {
		t.Fatalf("device state/input = %v/%t, want oauthDevice/false", updated.state, updated.inputActive)
	}
	if cmd == nil {
		t.Fatal("device mode did not start status polling")
	}
	rendered := updated.renderContent()
	for _, want := range []string{"https://auth.openai.com/codex/device", "ABCD-1234", "Device code:"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("device view missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "Callback URL:") {
		t.Fatalf("device view unexpectedly rendered callback input:\n%s", rendered)
	}
}

func TestOAuthTabKeepsBrowserCallbackModeWithoutDeviceCode(t *testing.T) {
	model := newOAuthTabModel(nil)
	model.SetSize(100, 30)
	updated, _ := model.Update(oauthStartMsg{
		url:          "https://auth.openai.com/oauth/authorize",
		state:        "browser-state",
		providerName: "Codex (OpenAI)",
		timeout:      time.Minute,
	})

	if updated.state != oauthRemote || !updated.inputActive {
		t.Fatalf("browser state/input = %v/%t, want oauthRemote/true", updated.state, updated.inputActive)
	}
}

func TestOAuthProvidersKeepDeviceDefaultAndBrowserFallback(t *testing.T) {
	var codexModes []string
	for _, provider := range oauthProviders {
		if provider.apiPath == "codex-auth-url" {
			codexModes = append(codexModes, provider.loginMode)
		}
	}
	if len(codexModes) != 2 || codexModes[0] != "" || codexModes[1] != "browser" {
		t.Fatalf("Codex provider modes = %#v, want default device followed by browser fallback", codexModes)
	}
	for _, provider := range oauthProviders {
		if provider.apiPath == "kimi-auth-url" || strings.EqualFold(provider.name, "kimi") {
			t.Fatalf("Kimi OAuth provider is still exposed: %#v", provider)
		}
	}
}
