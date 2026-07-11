package helps

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
)

// chatGPTCloudflareCookieJar mirrors the Codex client's process-wide cookie
// store. It deliberately accepts only Cloudflare infrastructure cookies and
// must never be expanded to retain ChatGPT account or session cookies.
type chatGPTCloudflareCookieJar struct {
	inner http.CookieJar
}

var (
	sharedChatGPTCloudflareCookieJarOnce sync.Once
	sharedChatGPTCloudflareCookieJar     http.CookieJar
)

// SharedChatGPTCloudflareCookieJar returns the process-wide, infrastructure-
// only cookie jar used by Codex HTTP and WebSocket transports.
func SharedChatGPTCloudflareCookieJar() http.CookieJar {
	sharedChatGPTCloudflareCookieJarOnce.Do(func() {
		sharedChatGPTCloudflareCookieJar = newChatGPTCloudflareCookieJar()
	})
	return sharedChatGPTCloudflareCookieJar
}

func newChatGPTCloudflareCookieJar() http.CookieJar {
	jar, _ := cookiejar.New(nil)
	return &chatGPTCloudflareCookieJar{inner: jar}
}

func (j *chatGPTCloudflareCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if j == nil || j.inner == nil || !isChatGPTCloudflareCookieURL(u) {
		return
	}
	allowed := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie != nil && isAllowedChatGPTCloudflareCookieName(cookie.Name) {
			allowed = append(allowed, cookie)
		}
	}
	if len(allowed) > 0 {
		j.inner.SetCookies(u, allowed)
	}
}

func (j *chatGPTCloudflareCookieJar) Cookies(u *url.URL) []*http.Cookie {
	if j == nil || j.inner == nil || !isChatGPTCloudflareCookieURL(u) {
		return nil
	}
	cookies := j.inner.Cookies(u)
	allowed := cookies[:0]
	for _, cookie := range cookies {
		if cookie != nil && isAllowedChatGPTCloudflareCookieName(cookie.Name) {
			allowed = append(allowed, cookie)
		}
	}
	return allowed
}

// AddChatGPTCloudflareCookies adds stored infrastructure cookies to a
// WebSocket upgrade request. net/http clients perform this step through Jar
// automatically; gorilla/websocket requires it explicitly.
func AddChatGPTCloudflareCookies(headers http.Header, rawURL string) {
	addChatGPTCloudflareCookies(headers, rawURL, SharedChatGPTCloudflareCookieJar())
}

func addChatGPTCloudflareCookies(headers http.Header, rawURL string, jar http.CookieJar) {
	if headers == nil {
		return
	}
	u := chatGPTCookieURL(rawURL)
	if u == nil || jar == nil {
		return
	}
	req := &http.Request{Header: headers}
	for _, cookie := range jar.Cookies(u) {
		req.AddCookie(cookie)
	}
}

// StoreChatGPTCloudflareCookies records cookies returned by a WebSocket
// handshake. Filtering is enforced again by the shared jar.
func StoreChatGPTCloudflareCookies(rawURL string, cookies []*http.Cookie) {
	storeChatGPTCloudflareCookies(rawURL, cookies, SharedChatGPTCloudflareCookieJar())
}

func storeChatGPTCloudflareCookies(rawURL string, cookies []*http.Cookie, jar http.CookieJar) {
	u := chatGPTCookieURL(rawURL)
	if u == nil || len(cookies) == 0 || jar == nil {
		return
	}
	jar.SetCookies(u, cookies)
}

func chatGPTCookieURL(rawURL string) *url.URL {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u == nil {
		return nil
	}
	switch strings.ToLower(u.Scheme) {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	if !isChatGPTCloudflareCookieURL(u) {
		return nil
	}
	return u
}

func isChatGPTCloudflareCookieURL(u *url.URL) bool {
	if u == nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	switch host {
	case "chatgpt.com", "chat.openai.com", "chatgpt-staging.com":
		return true
	default:
		return strings.HasSuffix(host, ".chatgpt.com") || strings.HasSuffix(host, ".chatgpt-staging.com")
	}
}

func isAllowedChatGPTCloudflareCookieName(name string) bool {
	name = strings.TrimSpace(name)
	switch name {
	case "__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob":
		return true
	default:
		return strings.HasPrefix(name, "cf_chl_")
	}
}
