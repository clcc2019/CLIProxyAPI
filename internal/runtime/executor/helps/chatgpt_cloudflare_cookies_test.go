package helps

import (
	"net/http"
	"net/url"
	"testing"
)

func TestChatGPTCloudflareCookieJarStoresOnlyInfrastructureCookies(t *testing.T) {
	jar := newChatGPTCloudflareCookieJar()
	u, err := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	jar.SetCookies(u, []*http.Cookie{
		{Name: "__cf_bm", Value: "bot-management"},
		{Name: "cf_chl_token", Value: "challenge"},
		{Name: "session-token", Value: "must-not-leak"},
	})

	cookies := jar.Cookies(u)
	if len(cookies) != 2 {
		t.Fatalf("cookie count = %d, want 2: %#v", len(cookies), cookies)
	}
	for _, cookie := range cookies {
		if cookie.Name == "session-token" {
			t.Fatal("account/session cookie was retained")
		}
	}
}

func TestChatGPTCloudflareCookieJarRejectsUntrustedHostsAndPlainHTTP(t *testing.T) {
	jar := newChatGPTCloudflareCookieJar()
	trusted, _ := url.Parse("https://chatgpt.com/")
	jar.SetCookies(trusted, []*http.Cookie{{Name: "cf_clearance", Value: "clearance"}})

	for _, rawURL := range []string{
		"http://chatgpt.com/",
		"https://evilchatgpt.com/",
		"https://chatgpt.com.evil.example/",
		"https://api.openai.com/",
		"https://foo.chat.openai.com/",
	} {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("url.Parse(%q) error = %v", rawURL, err)
		}
		jar.SetCookies(u, []*http.Cookie{{Name: "__cf_bm", Value: "bad"}})
		if cookies := jar.Cookies(u); len(cookies) != 0 {
			t.Fatalf("Cookies(%q) = %#v, want none", rawURL, cookies)
		}
	}
}

func TestChatGPTCookieURLConvertsOnlySecureWebsocketHosts(t *testing.T) {
	converted := chatGPTCookieURL("wss://api.chatgpt.com/backend-api/codex/responses")
	if converted == nil || converted.Scheme != "https" || converted.Hostname() != "api.chatgpt.com" {
		t.Fatalf("chatGPTCookieURL() = %v, want secure ChatGPT URL", converted)
	}
	if got := chatGPTCookieURL("ws://chatgpt.com/backend-api/codex/responses"); got != nil {
		t.Fatalf("plain websocket URL converted to %v, want nil", got)
	}
}

func TestChatGPTCloudflareCookiesBridgeWebsocketHandshake(t *testing.T) {
	jar := newChatGPTCloudflareCookieJar()
	wsURL := "wss://chatgpt.com/backend-api/codex/responses"
	storeChatGPTCloudflareCookies(wsURL, []*http.Cookie{
		{Name: "_cfuvid", Value: "visitor", Path: "/"},
		{Name: "account_session", Value: "must-not-leak", Path: "/"},
	}, jar)

	headers := make(http.Header)
	addChatGPTCloudflareCookies(headers, wsURL, jar)
	if got := headers.Get("Cookie"); got != "_cfuvid=visitor" {
		t.Fatalf("Cookie = %q, want only Cloudflare infrastructure cookie", got)
	}
}

func TestNewProxyAwareHTTPClientUsesSharedCloudflareCookieJar(t *testing.T) {
	client := newHTTPClient(http.DefaultTransport, 0)
	if client.Jar == nil {
		t.Fatal("expected HTTP client to use shared Cloudflare cookie jar")
	}
	if client.Jar != SharedChatGPTCloudflareCookieJar() {
		t.Fatal("expected process-wide Cloudflare cookie jar")
	}
}
