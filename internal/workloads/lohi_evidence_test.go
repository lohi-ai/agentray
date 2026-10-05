package workloads

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestLohiEvidenceV1ConfigContract(t *testing.T) {
	type export struct {
		Table  string   `json:"table"`
		Key    string   `json:"key"`
		Mode   string   `json:"mode"`
		Fields []string `json:"fields"`
	}
	var manifest struct {
		Version        string                         `json:"version"`
		SkillSHA256    string                         `json:"skill_sha256"`
		ConnectorID    string                         `json:"connector_id"`
		Normalizations map[string]map[string][]string `json:"normalizations"`
		Exports        []export                       `json:"exports"`
		Recipes        []string                       `json:"recipes"`
		RecipeSHA256   map[string]string              `json:"recipe_sha256"`
		Board          struct {
			Mode               string   `json:"mode"`
			OverwriteUserEdits bool     `json:"overwrite_user_edits"`
			RecipeRefs         []string `json:"recipe_refs"`
		} `json:"board"`
		Install struct {
			Preset            string `json:"preset"`
			Skill             string `json:"skill"`
			ExternalMCPExport string `json:"external_mcp_export"`
			Reinstall         string `json:"reinstall"`
		} `json:"install"`
	}
	if err := json.Unmarshal(LohiEvidenceManifest(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != LohiEvidenceVersion || manifest.Install.Preset != "data-analyst" || manifest.Install.Skill != LohiEvidenceVersion {
		t.Fatalf("install identity drifted: %+v", manifest.Install)
	}
	if manifest.Install.ExternalMCPExport != "SKILL.md" || manifest.Install.Reinstall != "idempotent_no_overwrite" {
		t.Fatalf("portable/reinstall contract drifted: %+v", manifest.Install)
	}
	if got := strings.Join(manifest.Normalizations["wallet_ledger_v1.reason"]["topup_purchase"], ","); got != "topup,topup_apple_consented,topup_purchase" {
		t.Fatalf("topup reason normalization drifted: %q", got)
	}
	skill := LohiEvidenceSkill()
	digest := sha256.Sum256([]byte(skill.Body))
	if got := hex.EncodeToString(digest[:]); got != manifest.SkillSHA256 {
		t.Fatalf("skill sha256 = %s, manifest = %s", got, manifest.SkillSHA256)
	}

	wantTables := []string{
		"ar_lohi.users_v1", "ar_lohi.topups_v1", "ar_lohi.wallet_ledger_v1",
		"ar_lohi.tts_daily_v1", "ar_lohi.passes_v1", "ar_lohi.reader_days_v1",
	}
	if len(manifest.Exports) != len(wantTables) {
		t.Fatalf("export count = %d, want %d", len(manifest.Exports), len(wantTables))
	}
	for i, item := range manifest.Exports {
		if item.Table != wantTables[i] || item.Key == "" || item.Mode == "" || len(item.Fields) == 0 {
			t.Fatalf("export %d malformed: %+v", i, item)
		}
		for _, field := range item.Fields {
			switch strings.ToLower(field) {
			case "email", "name", "oauth_id", "oauth_token", "access_token", "refresh_token", "dsn", "password":
				t.Errorf("export %s leaks forbidden field %s", item.Table, field)
			}
		}
	}

	recipes := LohiEvidenceRecipes()
	for _, ref := range manifest.Recipes {
		sqlText := recipes[ref]
		if !strings.Contains(sqlText, "connector_id = '"+manifest.ConnectorID+"'") || !strings.Contains(sqlText, "table_name") {
			t.Errorf("%s is not bound by connector and table", ref)
		}
		digest := sha256.Sum256([]byte(sqlText))
		if got := hex.EncodeToString(digest[:]); got != manifest.RecipeSHA256[ref] {
			t.Errorf("%s sha256 = %s, manifest = %s", ref, got, manifest.RecipeSHA256[ref])
		}
	}
	if manifest.Board.Mode != "extend_existing_only" || manifest.Board.OverwriteUserEdits || len(manifest.Board.RecipeRefs) != 11 {
		t.Fatalf("saved-board declaration is unsafe: %+v", manifest.Board)
	}

	dataAnalyst := MustBySlug("data-analyst")
	installed := 0
	for _, candidate := range dataAnalyst.Skills {
		if candidate.Name == LohiEvidenceVersion {
			installed++
			if candidate.Body != skill.Body {
				t.Error("Data Analyst does not install the portable skill verbatim")
			}
		}
	}
	if installed != 1 {
		t.Fatalf("Data Analyst has %d %s skills, want exactly 1", installed, LohiEvidenceVersion)
	}
	if _, ok := BySlug(LohiEvidenceVersion); ok {
		t.Fatal("Lohi evidence was registered as a new persona instead of a skill")
	}
}

func TestLohiEvidenceV1HonestyRulesStayPortable(t *testing.T) {
	body := LohiEvidenceSkill().Body
	for _, marker := range []string{
		"partial", "unavailable", "gross VND", "net event", "LT credits",
		"canonical_id", "Unknown signup attribution", "association", "scenario projection",
		"first-touch-with-unknown", "full elapsed horizon", "lt_purchased_topup_control",
		"settled escrow consumption", "direct-debit lower bounds",
		"do not", "read-only", "revision conflict", "external MCP clients",
	} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(marker)) {
			t.Errorf("portable skill missing honesty rule %q", marker)
		}
	}
}
