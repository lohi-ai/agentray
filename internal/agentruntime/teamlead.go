package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// Team-lead injection (ARCHITECT-AGENT-TEAM P2/P3). When the running agent is
// the selected lead of one or more teams, the runner extends its run — config
// only, no new runtime — with:
//
//  1. the team member roster merged into the spawn_subagent delegate list, so
//     the lead hands cards to members through the already-shipped delegation
//     path (each member runs under its OWN persona/tools/policy/secrets);
//  2. a synthetic "team-orchestrator" skill teaching the board workflow
//     (progressive disclosure: header always advertised, body loaded on
//     demand like any stored skill);
//  3. the team_board tool, scoped to the led teams only, so card state moves
//     are observable on the shared board.
//
// Non-lead members get none of this: membership grants no tools and no skill
// (safety rule), and the injection is re-derived per run, so demoting a lead
// revokes everything on its next run with no stored state to clean up.

// ToolTeamBoard is the stable name of the lead-only kanban tool.
const ToolTeamBoard = "team_board"

// orchestratorSkillID is the synthetic skill's id. Stored skill ids are UUIDs,
// so this name can never collide with one.
const orchestratorSkillID = "team-orchestrator"

const orchestratorSkillDescription = "Lead your team's kanban board: pick cards, break work down, " +
	"delegate tasks to team members, track status, and move cards as work progresses. " +
	"Load when asked to work the board, coordinate the team, or handle team work items."

const orchestratorSkillBody = `# Team orchestrator

You are the lead of the team(s) listed below. Work the shared kanban board with the team_board tool and delegate execution to your members with spawn_subagent.

## Workflow
1. **See the board**: team_board {"action":"list"} shows every card by column (backlog / doing / review / done) with ids and assignees.
2. **Pick a card**: choose the highest-value backlog card (or the one the user named). Move it to doing and assign it: team_board {"action":"move","card_id":"…","status":"doing","assignee":"<member name>"}.
3. **Delegate**: hand the work to the assigned member with spawn_subagent {"agent":"<member name>","task":"…"}. State the task fully and self-contained — the member sees nothing of this conversation. Pick the member whose specialty matches; use "self" only for work squarely in your own lane.
4. **Review**: read the member's answer. If acceptable, move the card to review (when the user should sign off) or done; if not, re-delegate with concrete corrections or finish it yourself.
5. **Synthesize**: report per card what was done, by whom, and the outcome. Create follow-up cards with team_board {"action":"add","title":"…","body":"…"} when work uncovers new work.

## Rules
- One card at a time unless asked to sweep the board; never leave a card in doing at the end of a turn — move it forward or back with a note in your reply.
- Members run under their own permissions. A member failing a task for a missing capability is a finding to report, not something to work around by doing it yourself with elevated access.
- Do not fabricate board state: every status change goes through team_board so the human sees the same board you do.

## Your teams
`

// teamBoardTool is the lead's kanban surface, bound to the teams the running
// agent leads. It deliberately supports only list/add/move — structural
// changes (roster, lead, deleting cards) stay human-only in the UI.
type teamBoardTool struct {
	store *storage.Store
	teams []storage.TeamLeadership
}

func (t *teamBoardTool) Name() string { return ToolTeamBoard }

func (t *teamBoardTool) Schema() agentcore.ToolSchema {
	names := make([]string, 0, len(t.teams))
	for _, tm := range t.teams {
		names = append(names, tm.Name)
	}
	return agentcore.ToolSchema{
		Name: ToolTeamBoard,
		Description: "Read and update the kanban board of the team(s) you lead (" + strings.Join(names, "; ") + "). " +
			"Actions: list (the full board), add (new backlog card), move (change a card's status and/or assignee).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"enum":        []string{"list", "add", "move"},
					"description": "list = show the board; add = create a backlog card; move = update a card's status/assignee.",
				},
				"team": map[string]any{
					"type":        "string",
					"description": "Team name or slug. Optional when you lead exactly one team.",
				},
				"card_id": map[string]any{
					"type":        "string",
					"description": "The card to move (from list). Required for move.",
				},
				"title": map[string]any{"type": "string", "description": "New card title. Required for add."},
				"body":  map[string]any{"type": "string", "description": "New card details (optional)."},
				"status": map[string]any{
					"type":        "string",
					"enum":        storage.TeamCardStatuses(),
					"description": "Target column for move.",
				},
				"assignee": map[string]any{
					"type":        "string",
					"description": "Member name (or slug) to assign the card to; \"\" clears the assignee. Optional.",
				},
			},
			"required": []string{"action"},
		},
	}
}

// teamBoardArgs is the decoded argument shape.
type teamBoardArgs struct {
	Action   string  `json:"action"`
	Team     string  `json:"team"`
	CardID   string  `json:"card_id"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	Status   string  `json:"status"`
	Assignee *string `json:"assignee"`
}

// resolveTeam picks the target team by name/slug, defaulting when there is
// exactly one.
func (t *teamBoardTool) resolveTeam(name string) (storage.TeamLeadership, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if len(t.teams) == 1 {
			return t.teams[0], nil
		}
		return storage.TeamLeadership{}, fmt.Errorf("you lead %d teams — pass the team parameter", len(t.teams))
	}
	for _, tm := range t.teams {
		if strings.EqualFold(tm.Name, name) || strings.EqualFold(tm.Slug, name) {
			return tm, nil
		}
	}
	return storage.TeamLeadership{}, fmt.Errorf("unknown team %q — you lead: %s", name, teamNames(t.teams))
}

func teamNames(teams []storage.TeamLeadership) string {
	names := make([]string, 0, len(teams))
	for _, tm := range teams {
		names = append(names, tm.Name)
	}
	return strings.Join(names, ", ")
}

// resolveMember maps a member name/slug/id to the member's agent id, so the
// model can assign by the same name it delegates with. Only the led team's
// roster resolves — an arbitrary agent id is refused.
func resolveMember(team storage.TeamLeadership, who string) (string, error) {
	who = strings.TrimSpace(who)
	if who == "" {
		return "", nil
	}
	for _, m := range team.Members {
		if strings.EqualFold(m.Name, who) || strings.EqualFold(m.Slug, who) || m.AgentID == who {
			return m.AgentID, nil
		}
	}
	return "", fmt.Errorf("unknown member %q on team %s — members: %s", who, team.Name, memberNames(team.Members))
}

func memberNames(members []storage.AgentDelegateTarget) string {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func (t *teamBoardTool) Run(ctx context.Context, args string) (string, error) {
	var in teamBoardArgs
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	team, err := t.resolveTeam(in.Team)
	if err != nil {
		return "", err
	}
	switch in.Action {
	case "list":
		cards, err := t.store.TeamCardsForRun(ctx, team.TeamID)
		if err != nil {
			return "", err
		}
		return renderBoard(team, cards), nil
	case "add":
		card, err := t.store.CreateTeamCardForRun(ctx, team.TeamID, in.Title, in.Body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("created card %s in backlog: %s", card.ID, card.Title), nil
	case "move":
		if strings.TrimSpace(in.CardID) == "" {
			return "", fmt.Errorf("card_id is required for move")
		}
		upd := storage.TeamCardUpdate{}
		if s := strings.TrimSpace(in.Status); s != "" {
			upd.Status = &s
		}
		if in.Assignee != nil {
			assigneeID, err := resolveMember(team, *in.Assignee)
			if err != nil {
				return "", err
			}
			upd.AssigneeAgentID = &assigneeID
		}
		if upd.Status == nil && upd.AssigneeAgentID == nil {
			return "", fmt.Errorf("move needs a status and/or assignee")
		}
		card, err := t.store.UpdateTeamCardForRun(ctx, team.TeamID, in.CardID, upd)
		if err != nil {
			return "", err
		}
		who := card.AssigneeName
		if who == "" {
			who = "unassigned"
		}
		return fmt.Sprintf("card %s → %s (%s): %s", card.ID, card.Status, who, card.Title), nil
	default:
		return "", fmt.Errorf("unknown action %q (list|add|move)", in.Action)
	}
}

// renderBoard formats the kanban compactly, column by column.
func renderBoard(team storage.TeamLeadership, cards []storage.TeamCard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Board of team %s (%d cards)\n", team.Name, len(cards))
	byStatus := map[string][]storage.TeamCard{}
	for _, c := range cards {
		byStatus[c.Status] = append(byStatus[c.Status], c)
	}
	for _, status := range storage.TeamCardStatuses() {
		fmt.Fprintf(&b, "\n## %s (%d)\n", status, len(byStatus[status]))
		for _, c := range byStatus[status] {
			who := c.AssigneeName
			if who == "" {
				who = "unassigned"
			}
			fmt.Fprintf(&b, "- [%s] %s — %s\n", c.ID, c.Title, who)
			if body := strings.TrimSpace(c.Body); body != "" {
				if r := []rune(body); len(r) > 200 {
					body = string(r[:200]) + "…"
				}
				fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(body, "\n", " "))
			}
		}
	}
	fmt.Fprintf(&b, "\nMembers: %s\n", memberNames(team.Members))
	return b.String()
}

// mergeTeamDelegates folds the led teams' member rosters into the delegate
// list. spawn_subagent matches delegates by name (case-insensitive), so
// dedupe is by folded name: an explicit agent_delegates grant wins over a
// team-derived entry of the same name, and a member on two led teams appears
// once. Pure, so the precedence is unit-testable without a DB.
func mergeTeamDelegates(existing []agentcore.Delegate, teams []storage.TeamLeadership, runFor func(agentID string) func(context.Context, string, agentcore.StreamSink) (string, agentcore.Usage, error)) []agentcore.Delegate {
	seen := make(map[string]bool, len(existing))
	for _, d := range existing {
		seen[strings.ToLower(d.Name)] = true
	}
	out := existing
	for _, team := range teams {
		for _, m := range team.Members {
			key := strings.ToLower(m.Name)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, agentcore.Delegate{
				Name:        m.Name,
				Description: m.Description,
				Run:         runFor(m.AgentID),
			})
		}
	}
	return out
}

// orchestratorSkill builds the synthetic lead-only skill: a header for the
// definition plus the full body (workflow + the concrete team rosters). Pure.
func orchestratorSkill(teams []storage.TeamLeadership) (agentcore.Skill, string) {
	var b strings.Builder
	b.WriteString(orchestratorSkillBody)
	for _, team := range teams {
		fmt.Fprintf(&b, "\n### %s\nMembers: %s\n", team.Name, memberNames(team.Members))
	}
	return agentcore.Skill{
		ID:          orchestratorSkillID,
		Name:        "Team orchestrator",
		Description: orchestratorSkillDescription,
		Enabled:     true,
	}, b.String()
}

// withOrchestratorBody wraps a SkillLoader so the synthetic skill's body is
// served from memory while every stored skill still loads through the inner
// loader. Pure.
func withOrchestratorBody(inner agentcore.SkillLoader, body string) agentcore.SkillLoader {
	return func(ctx context.Context, ids []string) (map[string]string, error) {
		rest := make([]string, 0, len(ids))
		wantOrchestrator := false
		for _, id := range ids {
			if id == orchestratorSkillID {
				wantOrchestrator = true
				continue
			}
			rest = append(rest, id)
		}
		out := map[string]string{}
		if len(rest) > 0 && inner != nil {
			loaded, err := inner(ctx, rest)
			if err != nil {
				return nil, err
			}
			for k, v := range loaded {
				out[k] = v
			}
		}
		if wantOrchestrator {
			out[orchestratorSkillID] = body
		}
		return out, nil
	}
}
