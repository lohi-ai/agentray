// Package credential preserves the AgentRay host API while sharing the vault
// implementation with other agentcore consumers. Environment loading stays local.
package credential

import shared "github.com/lohi-ai/agentray/credential"

type Vault = shared.Vault

func NewVault() *Vault                                 { return shared.NewVault() }
func ValidName(name string) bool                       { return shared.ValidName(name) }
func FromMap(values map[string]string) (*Vault, error) { return shared.FromMap(values) }
