package main

import (
	"context"
	"fmt"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

// Sanity ceiling against a hallucinated or malicious replica count.
const maxScaleFleetReplicas = 100_000

type ScaleFleetInput struct {
	Name      string `json:"name" jsonschema:"Fleet name"`
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific Fleet, so there's no 'all namespaces' option)"`
	Replicas  int32  `json:"replicas" jsonschema:"Target replica count; must be >= 0 and <= 100000"`
	Cluster   string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type ScaleFleetOutput struct {
	Fleet            string `json:"fleet"`
	PreviousReplicas int32  `json:"previousReplicas"`
	TargetReplicas   int32  `json:"targetReplicas"`
	Allocated        int32  `json:"allocated"`
	Note             string `json:"note"`
}

func (s *server) scaleFleet(ctx context.Context, req *mcp.CallToolRequest, in ScaleFleetInput) (*mcp.CallToolResult, ScaleFleetOutput, error) {
	if in.Replicas < 0 {
		return nil, ScaleFleetOutput{}, fmt.Errorf("replicas must be >= 0, got %d", in.Replicas)
	}
	if in.Replicas > maxScaleFleetReplicas {
		return nil, ScaleFleetOutput{}, fmt.Errorf("replicas %d exceeds the sanity ceiling of %d", in.Replicas, maxScaleFleetReplicas)
	}
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, ScaleFleetOutput{}, err
	}

	var out ScaleFleetOutput
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fleet, err := cl.agones.AgonesV1().Fleets(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		prev := fleet.Spec.Replicas
		fleet.Spec.Replicas = in.Replicas
		if _, err := cl.agones.AgonesV1().Fleets(in.Namespace).Update(ctx, fleet, metav1.UpdateOptions{}); err != nil {
			return err
		}
		note := "scaled"
		if in.Replicas < prev {
			note = "scaled down; Agones removes Ready servers first and does not disrupt Allocated servers"
		}
		out = ScaleFleetOutput{
			Fleet:            in.Name,
			PreviousReplicas: prev,
			TargetReplicas:   in.Replicas,
			Allocated:        fleet.Status.AllocatedReplicas,
			Note:             note,
		}
		return nil
	})
	if err != nil {
		return nil, ScaleFleetOutput{}, err
	}
	return nil, out, nil
}

// CounterSelectorInput and the sibling *SelectorInput/*ActionInput types below
// mirror Agones's own CountsAndLists allocation API (allocation.agones.dev's
// CounterSelector/ListSelector/CounterAction/ListAction) rather than the
// SDK-only path a running GameServer uses to update its own counters: this is
// the one externally-safe, Agones-native way to read or change Counter/List
// state without racing the SDK sidecar's in-process cache.
type CounterSelectorInput struct {
	MinCount     int64 `json:"minCount,omitempty" jsonschema:"Only match GameServers with count >= this; 0 means no lower bound"`
	MaxCount     int64 `json:"maxCount,omitempty" jsonschema:"Only match GameServers with count <= this; 0 means no upper bound"`
	MinAvailable int64 `json:"minAvailable,omitempty" jsonschema:"Only match GameServers with available capacity (capacity - count) >= this; 0 means no lower bound"`
	MaxAvailable int64 `json:"maxAvailable,omitempty" jsonschema:"Only match GameServers with available capacity <= this; 0 means no upper bound"`
}

type ListSelectorInput struct {
	ContainsValue string `json:"containsValue,omitempty" jsonschema:"Only match GameServers whose list contains this value; omit to match on capacity alone"`
	MinAvailable  int64  `json:"minAvailable,omitempty" jsonschema:"Only match GameServers with available list capacity >= this; 0 means no lower bound"`
	MaxAvailable  int64  `json:"maxAvailable,omitempty" jsonschema:"Only match GameServers with available list capacity <= this; 0 means no upper bound"`
}

type CounterActionInput struct {
	Action   string `json:"action,omitempty" jsonschema:"'Increment' or 'Decrement'; requires amount. Omit both to only change capacity"`
	Amount   int64  `json:"amount,omitempty" jsonschema:"Positive amount to increment or decrement by; required if action is set"`
	Capacity *int64 `json:"capacity,omitempty" jsonschema:"Set the counter's maximum capacity to this value (0 is valid); omit to leave capacity unchanged"`
}

type ListActionInput struct {
	AddValues    []string `json:"addValues,omitempty" jsonschema:"Values to append to the list; duplicates are ignored"`
	DeleteValues []string `json:"deleteValues,omitempty" jsonschema:"Values to remove from the list; values not present are ignored"`
	Capacity     *int64   `json:"capacity,omitempty" jsonschema:"Set the list's maximum capacity (0-1000) to this value; omit to leave capacity unchanged"`
}

func buildCounterActions(in map[string]CounterActionInput) (map[string]allocationv1.CounterAction, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]allocationv1.CounterAction, len(in))
	for name, a := range in {
		var action allocationv1.CounterAction
		switch {
		case a.Action != "":
			if a.Action != "Increment" && a.Action != "Decrement" {
				return nil, fmt.Errorf("counterActions[%q].action must be \"Increment\" or \"Decrement\", got %q", name, a.Action)
			}
			if a.Amount <= 0 {
				return nil, fmt.Errorf("counterActions[%q].amount must be > 0 when action is set", name)
			}
			act, amt := a.Action, a.Amount
			action.Action, action.Amount = &act, &amt
		case a.Amount != 0:
			return nil, fmt.Errorf("counterActions[%q].amount was given without action", name)
		}
		if a.Capacity != nil {
			if *a.Capacity < 0 {
				return nil, fmt.Errorf("counterActions[%q].capacity must be >= 0, got %d", name, *a.Capacity)
			}
			action.Capacity = a.Capacity
		}
		out[name] = action
	}
	return out, nil
}

func buildListActions(in map[string]ListActionInput) (map[string]allocationv1.ListAction, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]allocationv1.ListAction, len(in))
	for name, a := range in {
		if a.Capacity != nil && (*a.Capacity < 0 || *a.Capacity > agonesv1.ListMaxCapacity) {
			return nil, fmt.Errorf("listActions[%q].capacity must be between 0 and %d, got %d", name, agonesv1.ListMaxCapacity, *a.Capacity)
		}
		if err := validateListValues(name, "addValues", a.AddValues); err != nil {
			return nil, err
		}
		if err := validateListValues(name, "deleteValues", a.DeleteValues); err != nil {
			return nil, err
		}
		out[name] = allocationv1.ListAction{AddValues: a.AddValues, DeleteValues: a.DeleteValues, Capacity: a.Capacity}
	}
	return out, nil
}

// A list can never hold more than ListMaxCapacity values, so a single action
// naming more than that is a mistake worth rejecting before it reaches etcd.
func validateListValues(name, field string, values []string) error {
	if int64(len(values)) > agonesv1.ListMaxCapacity {
		return fmt.Errorf("listActions[%q].%s has %d values; a list holds at most %d", name, field, len(values), agonesv1.ListMaxCapacity)
	}
	for _, v := range values {
		if len(v) > 128 {
			return fmt.Errorf("listActions[%q].%s contains a value longer than 128 bytes (%d)", name, field, len(v))
		}
	}
	return nil
}

func buildCounterSelectors(in map[string]CounterSelectorInput) (map[string]allocationv1.CounterSelector, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]allocationv1.CounterSelector, len(in))
	for name, sel := range in {
		if sel.MinCount < 0 || sel.MaxCount < 0 || sel.MinAvailable < 0 || sel.MaxAvailable < 0 {
			return nil, fmt.Errorf("counterSelectors[%q]: bounds must be >= 0", name)
		}
		out[name] = allocationv1.CounterSelector{
			MinCount: sel.MinCount, MaxCount: sel.MaxCount,
			MinAvailable: sel.MinAvailable, MaxAvailable: sel.MaxAvailable,
		}
	}
	return out, nil
}

func buildListSelectors(in map[string]ListSelectorInput) (map[string]allocationv1.ListSelector, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]allocationv1.ListSelector, len(in))
	for name, sel := range in {
		if sel.MinAvailable < 0 || sel.MaxAvailable < 0 {
			return nil, fmt.Errorf("listSelectors[%q]: bounds must be >= 0", name)
		}
		out[name] = allocationv1.ListSelector{
			ContainsValue: sel.ContainsValue,
			MinAvailable:  sel.MinAvailable, MaxAvailable: sel.MaxAvailable,
		}
	}
	return out, nil
}

// PlayerSelectorInput mirrors Agones's alpha PlayerAllocationFilter feature
// (allocation.agones.dev's PlayerSelector). There's no PlayerAction
// counterpart to CounterAction/ListAction: player IDs and capacity are only
// ever written from inside the running game server via the SDK
// (PlayerConnect/PlayerDisconnect/SetPlayerCapacity), so this tool can filter
// on player capacity at allocation time but can't change it.
type PlayerSelectorInput struct {
	MinAvailable int64 `json:"minAvailable,omitempty" jsonschema:"Only match GameServers with available player capacity (capacity - count) >= this; 0 means no lower bound"`
	MaxAvailable int64 `json:"maxAvailable,omitempty" jsonschema:"Only match GameServers with available player capacity <= this; 0 means no upper bound"`
}

func buildPlayerSelector(in *PlayerSelectorInput) (*allocationv1.PlayerSelector, error) {
	if in == nil {
		return nil, nil
	}
	if in.MinAvailable < 0 || in.MaxAvailable < 0 {
		return nil, fmt.Errorf("playerSelector: bounds must be >= 0")
	}
	return &allocationv1.PlayerSelector{MinAvailable: in.MinAvailable, MaxAvailable: in.MaxAvailable}, nil
}

type AllocateInput struct {
	Fleet            string                          `json:"fleet" jsonschema:"Fleet to allocate a server from"`
	Namespace        string                          `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific Fleet, so there's no 'all namespaces' option)"`
	CounterSelectors map[string]CounterSelectorInput `json:"counterSelectors,omitempty" jsonschema:"Only allocate a GameServer whose Counters match these filters, keyed by counter name (requires the CountsAndLists feature and counters declared on the GameServer template)"`
	ListSelectors    map[string]ListSelectorInput    `json:"listSelectors,omitempty" jsonschema:"Only allocate a GameServer whose Lists match these filters, keyed by list name"`
	PlayerSelector   *PlayerSelectorInput            `json:"playerSelector,omitempty" jsonschema:"Only allocate a GameServer whose available player capacity matches (requires the PlayerTracking and PlayerAllocationFilter alpha features)"`
	CounterActions   map[string]CounterActionInput   `json:"counterActions,omitempty" jsonschema:"Apply these changes to the allocated GameServer's Counters, keyed by counter name"`
	ListActions      map[string]ListActionInput      `json:"listActions,omitempty" jsonschema:"Apply these changes to the allocated GameServer's Lists, keyed by list name"`
	Cluster          string                          `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type AllocateOutput struct {
	State      string                            `json:"state"`
	GameServer string                            `json:"gameServer,omitempty"`
	Address    string                            `json:"address,omitempty"`
	Ports      []int32                           `json:"ports,omitempty"`
	Counters   map[string]agonesv1.CounterStatus `json:"counters,omitempty"`
	Lists      map[string]agonesv1.ListStatus    `json:"lists,omitempty"`
}

func (s *server) allocateGameServer(ctx context.Context, req *mcp.CallToolRequest, in AllocateInput) (*mcp.CallToolResult, AllocateOutput, error) {
	if in.Fleet == "" {
		return nil, AllocateOutput{}, fmt.Errorf("fleet is required")
	}
	counterActions, err := buildCounterActions(in.CounterActions)
	if err != nil {
		return nil, AllocateOutput{}, err
	}
	listActions, err := buildListActions(in.ListActions)
	if err != nil {
		return nil, AllocateOutput{}, err
	}
	counterSelectors, err := buildCounterSelectors(in.CounterSelectors)
	if err != nil {
		return nil, AllocateOutput{}, err
	}
	listSelectors, err := buildListSelectors(in.ListSelectors)
	if err != nil {
		return nil, AllocateOutput{}, err
	}
	playerSelector, err := buildPlayerSelector(in.PlayerSelector)
	if err != nil {
		return nil, AllocateOutput{}, err
	}

	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, AllocateOutput{}, err
	}
	alloc := &allocationv1.GameServerAllocation{
		Spec: allocationv1.GameServerAllocationSpec{
			Selectors: []allocationv1.GameServerSelector{{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{agonesv1.FleetNameLabel: in.Fleet},
				},
				Counters: counterSelectors,
				Lists:    listSelectors,
				Players:  playerSelector,
			}},
			Counters: counterActions,
			Lists:    listActions,
		},
	}
	result, err := cl.agones.AllocationV1().GameServerAllocations(in.Namespace).Create(ctx, alloc, metav1.CreateOptions{})
	if err != nil {
		return nil, AllocateOutput{}, err
	}
	out := AllocateOutput{State: string(result.Status.State)}
	if result.Status.State == allocationv1.GameServerAllocationAllocated {
		out.GameServer = result.Status.GameServerName
		out.Address = result.Status.Address
		for _, p := range result.Status.Ports {
			out.Ports = append(out.Ports, p.Port)
		}
		out.Counters = result.Status.Counters
		out.Lists = result.Status.Lists
	}
	return nil, out, nil
}

type DeleteGameServerInput struct {
	Name      string `json:"name" jsonschema:"GameServer name"`
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific GameServer, so there's no 'all namespaces' option)"`
	Force     bool   `json:"force,omitempty" jsonschema:"Set true only if you intend to disconnect any players currently on this server - required to delete an Allocated server with a live match"`
	Cluster   string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type DeleteGameServerOutput struct {
	Deleted bool   `json:"deleted"`
	State   string `json:"state"`
	Warning string `json:"warning,omitempty"`
}

// The ResourceVersion precondition closes a TOCTOU gap: without it, a
// server that goes Allocated between the Get and the Delete would still get
// deleted. A mismatch fails as a conflict, and RetryOnConflict re-checks
// Allocated/force against fresh state before trying again.
func (s *server) deleteGameServer(ctx context.Context, req *mcp.CallToolRequest, in DeleteGameServerInput) (*mcp.CallToolResult, DeleteGameServerOutput, error) {
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, DeleteGameServerOutput{}, err
	}

	var out DeleteGameServerOutput
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		gs, err := cl.agones.AgonesV1().GameServers(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		state := string(gs.Status.State)
		if gs.Status.State == agonesv1.GameServerStateAllocated && !in.Force {
			out = DeleteGameServerOutput{
				Deleted: false,
				State:   state,
				Warning: "refused: server is Allocated with a live match in progress; pass force=true to delete anyway",
			}
			return nil
		}
		deleteErr := cl.agones.AgonesV1().GameServers(in.Namespace).Delete(ctx, in.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{ResourceVersion: &gs.ResourceVersion},
		})
		if deleteErr != nil {
			return deleteErr
		}
		warning := ""
		if in.Force && gs.Status.State == agonesv1.GameServerStateAllocated {
			warning = fmt.Sprintf("force-deleted Allocated server; players on %s are being disconnected", gs.Status.Address)
		}
		out = DeleteGameServerOutput{Deleted: true, State: state, Warning: warning}
		return nil
	})
	if err != nil {
		return nil, DeleteGameServerOutput{}, err
	}
	return nil, out, nil
}
