package managementasset

import "testing"

func TestResolveReleaseURL(t *testing.T) {
	tests := []struct {
		name string
		repo string
		tag  string
		want string
	}{
		{
			name: "empty uses default latest release",
			repo: "",
			tag:  "",
			want: defaultManagementReleaseURL,
		},
		{
			name: "repo root defaults to latest tag",
			repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center",
			tag:  "",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest",
		},
		{
			name: "release page latest stays latest",
			repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/latest",
			tag:  "",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest",
		},
		{
			name: "release page tag is preserved",
			repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/tag/v1.2.3",
			tag:  "",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/v1.2.3",
		},
		{
			name: "api repo root defaults to latest tag",
			repo: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center",
			tag:  "",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest",
		},
		{
			name: "api tag endpoint is preserved",
			repo: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/v1.2.3",
			tag:  "",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/v1.2.3",
		},
		{
			name: "explicit tag overrides latest release url",
			repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/latest",
			tag:  "v2.0.0",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/v2.0.0",
		},
		{
			name: "explicit tag overrides embedded tag",
			repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/tag/v1.2.3",
			tag:  "v2.0.0",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/v2.0.0",
		},
		{
			name: "explicit tag uses default repository when repo is empty",
			repo: "",
			tag:  "v3.1.4",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/v3.1.4",
		},
		{
			name: "slash tag in release page is preserved",
			repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/tag/release/candidate",
			tag:  "",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/release%2Fcandidate",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveReleaseURL(tc.repo, tc.tag); got != tc.want {
				t.Fatalf("resolveReleaseURL(%q, %q) = %q, want %q", tc.repo, tc.tag, got, tc.want)
			}
		})
	}
}
