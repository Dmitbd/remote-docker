// Package tray maps background-agent status to a small, platform-neutral tray menu.
package tray

import "github.com/Dmitbd/remote-docker/internal/localapi"

type Icon string

const (
	IconUnpaired    Icon = "unpaired"
	IconConnecting  Icon = "connecting"
	IconStarting    Icon = "starting"
	IconSyncing     Icon = "syncing"
	IconReady       Icon = "ready"
	IconDegraded    Icon = "degraded"
	IconNeedsAction Icon = "needs-action"
)

type Action string

const (
	ActionPair           Action = "pair"
	ActionOpenStatus     Action = "open-status"
	ActionAddWorkspace   Action = "add-workspace"
	ActionRetry          Action = "retry"
	ActionRunDiagnostics Action = "run-diagnostics"
	ActionUnpair         Action = "unpair"
	ActionQuit           Action = "quit"
	ActionConfirmPair    Action = "confirm-pair"
)

type Item struct {
	Action  Action
	Label   string
	Enabled bool
}

type Pairing struct {
	DeviceName string
	Code       string
	sessionID  string
}

type Model struct {
	Label      string
	Message    string
	Icon       Icon
	Items      []Item
	Candidates []localapi.PairingCandidate
	Pairing    *Pairing
}

// MenuForStatus produces stable labels, icon identifiers, and action items for
// every status returned by the owner-only local control API.
func MenuForStatus(status localapi.StatusResult) Model {
	model := Model{Message: status.Message}
	switch status.State {
	case "Unpaired":
		model.Label, model.Icon = "Not paired", IconUnpaired
	case "Connecting":
		model.Label, model.Icon = "Connecting", IconConnecting
	case "EngineStarting":
		model.Label, model.Icon = "Starting Docker Engine", IconStarting
	case "Syncing":
		model.Label, model.Icon = "Syncing workspaces", IconSyncing
	case "Ready":
		model.Label, model.Icon = "Ready", IconReady
	case "Degraded":
		model.Label, model.Icon = "Connection needs attention", IconDegraded
	case "NeedsAction":
		model.Label, model.Icon = "Action required", IconNeedsAction
	default:
		model.Label, model.Icon = "Action required", IconNeedsAction
	}
	model.Items = baseItems(status.State, status.Paired)
	return model
}

func baseItems(state string, paired bool) []Item {
	return []Item{
		{Action: ActionPair, Label: "Pair", Enabled: !paired},
		{Action: ActionOpenStatus, Label: "Open status", Enabled: true},
		{Action: ActionAddWorkspace, Label: "Add workspace", Enabled: paired},
		{Action: ActionRetry, Label: "Retry", Enabled: state == "Degraded" || state == "NeedsAction"},
		{Action: ActionRunDiagnostics, Label: "Run diagnostics", Enabled: true},
		{Action: ActionUnpair, Label: "Unpair", Enabled: paired},
		{Action: ActionQuit, Label: "Quit UI", Enabled: true},
	}
}

func actionIDs(items []Item) []Action {
	actions := make([]Action, 0, len(items))
	for _, item := range items {
		actions = append(actions, item.Action)
	}
	return actions
}

func hasAction(items []Item, action Action) bool {
	for _, item := range items {
		if item.Action == action {
			return true
		}
	}
	return false
}

func containsDestructiveAction(items []Item) bool {
	for _, item := range items {
		switch item.Action {
		case ActionPair, ActionOpenStatus, ActionAddWorkspace, ActionRetry, ActionRunDiagnostics, ActionUnpair, ActionQuit, ActionConfirmPair:
			continue
		default:
			return true
		}
	}
	return false
}
