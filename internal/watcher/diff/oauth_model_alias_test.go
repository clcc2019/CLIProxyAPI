package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestDiffOAuthModelAliasChanges_DetectsReasoningEffortChange(t *testing.T) {
	oldAliases := map[string][]config.OAuthModelAlias{
		"codex": {{
			Name:            "gpt-5.6-terra",
			Alias:           "gpt-5.5",
			ReasoningEffort: map[string]string{"default": "high", "low": "medium"},
		}},
	}
	newAliases := map[string][]config.OAuthModelAlias{
		"codex": {{
			Name:            "gpt-5.6-terra",
			Alias:           "gpt-5.5",
			ReasoningEffort: map[string]string{"default": "high", "low": "high"},
		}},
	}

	changes, affected := DiffOAuthModelAliasChanges(oldAliases, newAliases)
	if len(changes) != 1 || changes[0] != "oauth-model-alias[codex]: updated (1 -> 1 entries)" {
		t.Fatalf("changes = %#v", changes)
	}
	if len(affected) != 1 || affected[0] != "codex" {
		t.Fatalf("affected = %#v", affected)
	}
}
