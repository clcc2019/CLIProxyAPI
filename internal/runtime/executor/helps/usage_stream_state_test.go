package helps

import (
	"context"
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestUsageReporterCarriesStreamState(t *testing.T) {
	for _, stream := range []bool{false, true} {
		ctx := coreusage.WithStream(context.Background(), stream)
		reporter := NewUsageReporter(ctx, "openai", "gpt-5", nil)
		if got := reporter.buildRecord(coreusage.Detail{TotalTokens: 1}, false).Stream; got != stream {
			t.Fatalf("stream = %v, want %v", got, stream)
		}
	}
}
