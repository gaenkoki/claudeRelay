package proxy

import "github.com/local/claude-relay/internal/store"

const (
	ingressCompatible   = "compatible"
	ingressExperimental = "experimental"
	ingressOfficial     = "official"
)

type ingressPolicy struct {
	Name                      string
	AccountAccess             store.AccountAccess
	RequireClaudeCode         bool
	NormalizeToolNames        bool
	AlignGeneratedAttribution bool
}

var (
	compatibleIngress = ingressPolicy{
		Name:          ingressCompatible,
		AccountAccess: store.AccountAccessCompatibleOnly,
	}
	experimentalIngress = ingressPolicy{
		Name:                      ingressExperimental,
		AccountAccess:             store.AccountAccessCompatibleOnly,
		NormalizeToolNames:        true,
		AlignGeneratedAttribution: true,
	}
	officialIngress = ingressPolicy{
		Name:              ingressOfficial,
		AccountAccess:     store.AccountAccessAll,
		RequireClaudeCode: true,
	}
)
