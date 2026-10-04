package workloads

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// LohiRevenueObserverVersion is the configuration contract for scheduled
// observation of the versioned Lohi evidence pack. It remains a skill plus
// trigger templates on the stock Insight Digest; it is not a new runtime.
const LohiRevenueObserverVersion = "lohi-revenue-observer-v1"

//go:embed config/lohi-revenue-observer-v1/SKILL.md
var lohiRevenueObserverSkillBody string

//go:embed config/lohi-revenue-observer-v1/manifest.json
var lohiRevenueObserverManifestJSON []byte

// LohiRevenueObserverSkill returns the portable operating contract installed
// on the existing Insight Digest preset.
func LohiRevenueObserverSkill() Skill {
	return Skill{
		Name:        LohiRevenueObserverVersion,
		Description: "Run gated daily completeness and weekly mature-cohort observations over lohi-evidence-v1 using existing AgentRay schedules, Plans, and delivery.",
		Body:        lohiRevenueObserverSkillBody,
	}
}

// LohiRevenueObserverManifest returns a copy of the disabled trigger templates
// and their UTC/local-time mapping. Enabling them remains an explicit operator
// action in Agent Garden after bindings and permissions are configured.
func LohiRevenueObserverManifest() []byte {
	return append([]byte(nil), lohiRevenueObserverManifestJSON...)
}

func init() {
	var manifest struct {
		Version           string `json:"version"`
		DefinitionVersion string `json:"definition_version"`
	}
	if err := json.Unmarshal(lohiRevenueObserverManifestJSON, &manifest); err != nil {
		panic(fmt.Sprintf("workloads: invalid %s manifest: %v", LohiRevenueObserverVersion, err))
	}
	if manifest.Version != LohiRevenueObserverVersion {
		panic(fmt.Sprintf("workloads: Lohi observer manifest version %q, want %q", manifest.Version, LohiRevenueObserverVersion))
	}
	if manifest.DefinitionVersion != LohiEvidenceVersion {
		panic(fmt.Sprintf("workloads: Lohi observer definition %q, want %q", manifest.DefinitionVersion, LohiEvidenceVersion))
	}
}
