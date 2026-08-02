package executor

import (
	"context"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// CountTokens uses the provider's native count_tokens endpoint. Claude's
// multimodal and document accounting cannot be reproduced accurately by a
// text-only local tokenizer.
func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.countTokensUpstream(ctx, auth, req, opts)
}
