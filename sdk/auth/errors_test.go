package auth

import "testing"

func TestProjectSelectionErrorError(t *testing.T) {
	tests := []struct {
		name string
		err  *ProjectSelectionError
		want string
	}{
		{name: "nil", err: nil, want: "cliproxy auth: project selection required"},
		{name: "empty email", err: &ProjectSelectionError{}, want: "cliproxy auth: project selection required"},
		{name: "with email", err: &ProjectSelectionError{Email: "user@example.com"}, want: "cliproxy auth: project selection required for user@example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
