package managementasset

import "testing"

func TestResolveReleaseURL(t *testing.T) {
	tests := []struct {
		name string
		repo string
		want string
	}{
		{
			name: "empty uses default latest release",
			repo: "",
			want: defaultManagementReleaseURL,
		},
		{
			name: "repo root defaults to latest tag",
			repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest",
		},
		{
			name: "release page latest stays latest",
			repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/latest",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest",
		},
		{
			name: "release page tag is preserved",
			repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/tag/v1.2.3",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/v1.2.3",
		},
		{
			name: "api repo root defaults to latest tag",
			repo: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest",
		},
		{
			name: "api tag endpoint is preserved",
			repo: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/v1.2.3",
			want: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/tags/v1.2.3",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveReleaseURL(tc.repo); got != tc.want {
				t.Fatalf("resolveReleaseURL(%q) = %q, want %q", tc.repo, got, tc.want)
			}
		})
	}
}
