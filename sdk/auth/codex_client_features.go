package auth

import (
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

const (
	codexDefaultClientOriginator = misc.CodexDefaultOriginator
	codexDefaultClientVersion    = misc.CodexCLIVersion
)

type CodexClientFeatures struct {
	InstallationID       string
	Originator           string
	UserAgent            string
	OriginatorExplicit   bool
	UserAgentExplicit    bool
	InstallationExplicit bool
}

func NewCodexClientFeatures(metadata map[string]string) CodexClientFeatures {
	originator := codexClientFeatureString(metadata, "originator", "Originator", "header:Originator", "header:originator")
	originatorExplicit := originator != ""
	if originator == "" {
		originator = codexDefaultClientOriginator
	}

	userAgent := codexClientFeatureString(metadata, "user_agent", "user-agent", "userAgent", "header:User-Agent", "header:user-agent")
	userAgentExplicit := userAgent != ""
	if userAgent == "" {
		userAgent = misc.CodexCLIUserAgentWithOriginatorAndVersion(originator, codexDefaultClientVersion)
	}

	installationID := codexClientFeatureString(
		metadata,
		"installation_id",
		"installation-id",
		"installationId",
		"codex_installation_id",
		"header:X-Codex-Installation-Id",
		"header:x-codex-installation-id",
		"x-codex-installation-id",
	)
	installationExplicit := installationID != ""
	if installationID == "" {
		installationID = uuid.NewString()
	}

	return CodexClientFeatures{
		InstallationID:       installationID,
		Originator:           originator,
		UserAgent:            userAgent,
		OriginatorExplicit:   originatorExplicit,
		UserAgentExplicit:    userAgentExplicit,
		InstallationExplicit: installationExplicit,
	}
}

func (f CodexClientFeatures) AddToMetadata(metadata map[string]any) {
	if metadata == nil {
		return
	}
	if f.InstallationID != "" {
		metadata["installation_id"] = f.InstallationID
	}
	if f.OriginatorExplicit && f.Originator != "" {
		metadata["originator"] = f.Originator
	}
	if f.UserAgentExplicit && f.UserAgent != "" {
		metadata["user_agent"] = f.UserAgent
	}
}

func (f CodexClientFeatures) AddToAttributes(attrs map[string]string) {
	if attrs == nil {
		return
	}
	if f.InstallationID != "" {
		attrs["header:X-Codex-Installation-Id"] = f.InstallationID
	}
	if f.OriginatorExplicit && f.Originator != "" {
		attrs["originator"] = f.Originator
		attrs["header:Originator"] = f.Originator
	}
	if f.UserAgentExplicit && f.UserAgent != "" {
		attrs["header:User-Agent"] = f.UserAgent
	}
}

func codexClientFeatureString(metadata map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			return value
		}
	}
	return ""
}
