package workloads

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// LohiEvidenceVersion is the stable skill/config contract consumed by the
// stock Data Analyst and by external MCP clients.
const LohiEvidenceVersion = "lohi-evidence-v1"

//go:embed config/lohi-evidence-v1/SKILL.md
var lohiEvidenceSkillBody string

//go:embed config/lohi-evidence-v1/manifest.json
var lohiEvidenceManifestJSON []byte

var lohiRecipeBlock = regexp.MustCompile(`(?s)<!-- recipe:(R[0-9]{2}) -->\s*` + "```sql\\s*(.*?)\\s*```")

// LohiEvidenceSkill returns the exact portable skill installed by the
// marketplace. External MCP clients export/import this same body verbatim.
func LohiEvidenceSkill() Skill {
	return Skill{
		Name:        LohiEvidenceVersion,
		Description: "Reproduce the governed Lohi revenue-drop evidence pack with C4 bindings and canonical R01-R11 SQL.",
		Body:        lohiEvidenceSkillBody,
	}
}

// LohiEvidenceManifest returns a copy so callers cannot mutate the embedded
// declaration. It is configuration data only; no Lohi-specific runtime path is
// introduced.
func LohiEvidenceManifest() []byte {
	return append([]byte(nil), lohiEvidenceManifestJSON...)
}

// LohiEvidenceRecipes extracts the canonical SQL verbatim from the portable
// skill. Keeping a single source of truth prevents the installed skill and the
// executable reconciliation suite from drifting.
func LohiEvidenceRecipes() map[string]string {
	out := make(map[string]string, 11)
	for _, match := range lohiRecipeBlock.FindAllStringSubmatch(lohiEvidenceSkillBody, -1) {
		out[match[1]] = strings.TrimSpace(match[2])
	}
	return out
}

func init() {
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(lohiEvidenceManifestJSON, &manifest); err != nil {
		panic(fmt.Sprintf("workloads: invalid %s manifest: %v", LohiEvidenceVersion, err))
	}
	if manifest.Version != LohiEvidenceVersion {
		panic(fmt.Sprintf("workloads: Lohi evidence manifest version %q, want %q", manifest.Version, LohiEvidenceVersion))
	}
	if recipes := LohiEvidenceRecipes(); len(recipes) != 11 {
		panic(fmt.Sprintf("workloads: %s has %d canonical recipes, want 11", LohiEvidenceVersion, len(recipes)))
	}
}
