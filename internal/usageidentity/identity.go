package usageidentity

// AccountIdentity is a private immutable copy of identity facts from a selected credential.
type AccountIdentity struct {
	family           string
	providerKey      string
	authID           string
	workspaceID      string
	memberID         string
	organizationUUID string
	accountUUID      string
	snapshot         bool
	subscription     bool
	plan             string
}

// NewSelected records the selected credential's identity facts before asynchronous dispatch.
func NewSelected(executorType, providerKey, authID, workspaceID, memberID, organizationUUID, accountUUID string) AccountIdentity {
	return AccountIdentity{
		family: Family(executorType, providerKey), providerKey: providerKey, authID: authID,
		workspaceID: workspaceID, memberID: memberID,
		organizationUUID: organizationUUID, accountUUID: accountUUID, snapshot: true,
	}
}

// WithSubscriptionPlan marks the credential as a subscription login and records its provider-local plan.
// The plan is descriptive only and never participates in identity derivation.
func (i AccountIdentity) WithSubscriptionPlan(plan string) AccountIdentity {
	i.subscription, i.plan = true, plan
	return i
}

func (i AccountIdentity) Family() string             { return i.family }
func (i AccountIdentity) ProviderKey() string        { return i.providerKey }
func (i AccountIdentity) AuthID() string             { return i.authID }
func (i AccountIdentity) WorkspaceID() string        { return i.workspaceID }
func (i AccountIdentity) MemberID() string           { return i.memberID }
func (i AccountIdentity) OrganizationUUID() string   { return i.organizationUUID }
func (i AccountIdentity) AccountUUID() string        { return i.accountUUID }
func (i AccountIdentity) IsCredentialSnapshot() bool { return i.snapshot }
func (i AccountIdentity) IsSubscription() bool       { return i.subscription }
func (i AccountIdentity) Plan() string               { return i.plan }
func (i AccountIdentity) String() string             { return "{account identity redacted}" }
func (i AccountIdentity) GoString() string           { return i.String() }

// Family returns an accepted provider family only for a known executor and identifier pair.
func Family(executorType, provider string) string {
	switch executorType {
	case "OpenAICompatExecutor":
		return "openai-compatibility"
	case "CodexExecutor", "CodexWebsocketsExecutor", "CodexAutoExecutor":
		if provider == "codex" {
			return "codex"
		}
	case "ClaudeExecutor":
		if provider == "claude" {
			return "claude"
		}
	case "GeminiExecutor":
		if provider == "gemini" || provider == "gemini-interactions" {
			return provider
		}
	case "GeminiVertexExecutor":
		if provider == "vertex" {
			return provider
		}
	case "AIStudioExecutor":
		if provider == "aistudio" {
			return provider
		}
	case "AntigravityExecutor":
		if provider == "antigravity" {
			return provider
		}
	case "KimiExecutor":
		if provider == "kimi" {
			return provider
		}
	case "XAIExecutor", "XAIWebsocketsExecutor", "XAIAutoExecutor":
		if provider == "xai" {
			return provider
		}
	case "DevinExecutor":
		if provider == "devin" {
			return provider
		}
	case "MetaExecutor":
		if provider == "meta" {
			return provider
		}
	}
	return ""
}
