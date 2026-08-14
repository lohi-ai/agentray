package app

import (
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/workloads"
)

// marketplacePresets adapts the workloads registry into the store DTO the
// install/seed path already understands. Composition root only — store does
// not import workloads.
func marketplacePresets() []storage.AgentPreset {
	packs := workloads.All()
	out := make([]storage.AgentPreset, len(packs))
	for i, p := range packs {
		out[i] = packToPreset(p)
	}
	return out
}

func marketplacePresetBySlug(slug string) (storage.AgentPreset, bool) {
	p, ok := workloads.BySlug(slug)
	if !ok {
		return storage.AgentPreset{}, false
	}
	return packToPreset(p), true
}

func packToPreset(p workloads.Pack) storage.AgentPreset {
	skills := make([]storage.AgentPresetSkill, len(p.Skills))
	for i, sk := range p.Skills {
		skills[i] = storage.AgentPresetSkill{Name: sk.Name, Description: sk.Description, Body: sk.Body}
	}
	return storage.AgentPreset{
		Slug:        p.Slug,
		Name:        p.Name,
		Tagline:     p.Tagline,
		Description: p.Description,
		Category:    string(p.Category),
		Icon:        p.Icon,
		SoulMD:      p.SoulMD,
		AgentsMD:    p.AgentsMD,
		Scopes:      p.Scopes,
		Skills:      skills,
	}
}
