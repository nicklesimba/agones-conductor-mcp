package main

import (
	"context"
	"fmt"

	"agones.dev/agones/pkg/apis"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultGameServerContainerName = "game-server"

type CreateFleetInput struct {
	Name           string                      `json:"name" jsonschema:"Fleet name"`
	Namespace      string                      `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific Fleet, so there's no 'all namespaces' option)"`
	Replicas       int32                       `json:"replicas" jsonschema:"Initial replica count; must be >= 0 and <= 100000"`
	Image          string                      `json:"image" jsonschema:"Container image for the game server, e.g. gcr.io/my-project/my-game:v1"`
	ContainerName  string                      `json:"containerName,omitempty" jsonschema:"Name for the game server container; defaults to game-server"`
	ContainerPort  int32                       `json:"containerPort" jsonschema:"Port the game server process listens on inside the container, 1-65535"`
	PortPolicy     string                      `json:"portPolicy,omitempty" jsonschema:"Dynamic, Static, Passthrough, or None; defaults to Dynamic"`
	Protocol       string                      `json:"protocol,omitempty" jsonschema:"Port protocol: UDP, TCP, or TCPUDP (both protocols on the same port number); defaults to UDP, matching Agones"`
	CPURequest     string                      `json:"cpuRequest,omitempty" jsonschema:"e.g. 100m; omit to leave unset"`
	CPULimit       string                      `json:"cpuLimit,omitempty" jsonschema:"e.g. 500m; omit to leave unset"`
	MemoryRequest  string                      `json:"memoryRequest,omitempty" jsonschema:"e.g. 128Mi; omit to leave unset"`
	MemoryLimit    string                      `json:"memoryLimit,omitempty" jsonschema:"e.g. 256Mi; omit to leave unset"`
	Scheduling     string                      `json:"scheduling,omitempty" jsonschema:"Packed or Distributed; defaults to Packed"`
	Counters       map[string]CounterInitInput `json:"counters,omitempty" jsonschema:"Initial Counters to declare on this fleet's GameServers, keyed by counter name (requires the CountsAndLists feature; Counter/List keys can only be declared here at creation time, not added later)"`
	Lists          map[string]ListInitInput    `json:"lists,omitempty" jsonschema:"Initial Lists to declare on this fleet's GameServers, keyed by list name"`
	PlayerCapacity int64                       `json:"playerCapacity,omitempty" jsonschema:"Initial player capacity for this fleet's GameServers (requires the PlayerTracking alpha feature); omit or 0 to leave player tracking off"`
	DryRun         bool                        `json:"dryRun,omitempty" jsonschema:"Validate server-side without creating anything; the response shows what would have been created"`
	Cluster        string                      `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type CounterInitInput struct {
	Count    int64 `json:"count,omitempty" jsonschema:"Initial count; must be >= 0 and <= capacity"`
	Capacity int64 `json:"capacity" jsonschema:"Maximum capacity; must be >= 0"`
}

type ListInitInput struct {
	Capacity int64    `json:"capacity" jsonschema:"Maximum capacity; must be between 0 and 1000"`
	Values   []string `json:"values,omitempty" jsonschema:"Initial values; must not exceed capacity"`
}

func buildInitialCounters(in map[string]CounterInitInput) (map[string]agonesv1.CounterStatus, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]agonesv1.CounterStatus, len(in))
	for name, c := range in {
		if c.Capacity < 0 {
			return nil, fmt.Errorf("counters[%q].capacity must be >= 0, got %d", name, c.Capacity)
		}
		if c.Count < 0 || c.Count > c.Capacity {
			return nil, fmt.Errorf("counters[%q].count must be >= 0 and <= capacity (%d), got %d", name, c.Capacity, c.Count)
		}
		out[name] = agonesv1.CounterStatus{Count: c.Count, Capacity: c.Capacity}
	}
	return out, nil
}

func buildInitialLists(in map[string]ListInitInput) (map[string]agonesv1.ListStatus, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]agonesv1.ListStatus, len(in))
	for name, l := range in {
		if l.Capacity < 0 || l.Capacity > agonesv1.ListMaxCapacity {
			return nil, fmt.Errorf("lists[%q].capacity must be between 0 and %d, got %d", name, agonesv1.ListMaxCapacity, l.Capacity)
		}
		if int64(len(l.Values)) > l.Capacity {
			return nil, fmt.Errorf("lists[%q] has %d initial values but capacity %d", name, len(l.Values), l.Capacity)
		}
		out[name] = agonesv1.ListStatus{Capacity: l.Capacity, Values: l.Values}
	}
	return out, nil
}

type CreateFleetOutput struct {
	Fleet  FleetSummary `json:"fleet"`
	DryRun bool         `json:"dryRun,omitempty" jsonschema:"True: nothing was actually created"`
}

func (s *server) createFleet(ctx context.Context, req *mcp.CallToolRequest, in CreateFleetInput) (*mcp.CallToolResult, CreateFleetOutput, error) {
	if in.Replicas < 0 || in.Replicas > maxScaleFleetReplicas {
		return nil, CreateFleetOutput{}, fmt.Errorf("replicas must be >= 0 and <= %d, got %d", maxScaleFleetReplicas, in.Replicas)
	}
	if in.Image == "" {
		return nil, CreateFleetOutput{}, fmt.Errorf("image is required")
	}
	if in.ContainerPort <= 0 || in.ContainerPort > 65535 {
		return nil, CreateFleetOutput{}, fmt.Errorf("containerPort must be between 1 and 65535, got %d", in.ContainerPort)
	}

	containerName := in.ContainerName
	if containerName == "" {
		containerName = defaultGameServerContainerName
	}

	portPolicy := agonesv1.Dynamic
	if in.PortPolicy != "" {
		switch agonesv1.PortPolicy(in.PortPolicy) {
		case agonesv1.Dynamic, agonesv1.Static, agonesv1.Passthrough, agonesv1.None:
			portPolicy = agonesv1.PortPolicy(in.PortPolicy)
		default:
			return nil, CreateFleetOutput{}, fmt.Errorf("portPolicy must be one of Dynamic, Static, Passthrough, None; got %q", in.PortPolicy)
		}
	}

	scheduling := apis.Packed
	if in.Scheduling != "" {
		switch apis.SchedulingStrategy(in.Scheduling) {
		case apis.Packed, apis.Distributed:
			scheduling = apis.SchedulingStrategy(in.Scheduling)
		default:
			return nil, CreateFleetOutput{}, fmt.Errorf("scheduling must be Packed or Distributed; got %q", in.Scheduling)
		}
	}

	protocol := corev1.ProtocolUDP
	if in.Protocol != "" {
		switch corev1.Protocol(in.Protocol) {
		case corev1.ProtocolUDP, corev1.ProtocolTCP, agonesv1.ProtocolTCPUDP:
			protocol = corev1.Protocol(in.Protocol)
		default:
			return nil, CreateFleetOutput{}, fmt.Errorf("protocol must be UDP, TCP, or TCPUDP; got %q", in.Protocol)
		}
	}

	resources, err := buildResourceRequirements(in.CPURequest, in.CPULimit, in.MemoryRequest, in.MemoryLimit)
	if err != nil {
		return nil, CreateFleetOutput{}, err
	}
	initialCounters, err := buildInitialCounters(in.Counters)
	if err != nil {
		return nil, CreateFleetOutput{}, err
	}
	initialLists, err := buildInitialLists(in.Lists)
	if err != nil {
		return nil, CreateFleetOutput{}, err
	}
	if in.PlayerCapacity < 0 {
		return nil, CreateFleetOutput{}, fmt.Errorf("playerCapacity must be >= 0, got %d", in.PlayerCapacity)
	}
	var players *agonesv1.PlayersSpec
	if in.PlayerCapacity > 0 {
		players = &agonesv1.PlayersSpec{InitialCapacity: in.PlayerCapacity}
	}

	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, CreateFleetOutput{}, err
	}

	fleet := &agonesv1.Fleet{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace},
		Spec: agonesv1.FleetSpec{
			Replicas:   in.Replicas,
			Scheduling: scheduling,
			Template: agonesv1.GameServerTemplateSpec{
				Spec: agonesv1.GameServerSpec{
					Container: containerName,
					Counters:  initialCounters,
					Lists:     initialLists,
					Players:   players,
					Ports: []agonesv1.GameServerPort{{
						Name:          "default",
						PortPolicy:    portPolicy,
						Protocol:      protocol,
						ContainerPort: in.ContainerPort,
					}},
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:      containerName,
								Image:     in.Image,
								Resources: resources,
							}},
						},
					},
				},
			},
		},
	}

	created, err := cl.agones.AgonesV1().Fleets(in.Namespace).Create(ctx, fleet, metav1.CreateOptions{DryRun: dryRunOpt(in.DryRun)})
	if err != nil {
		return nil, CreateFleetOutput{}, fmt.Errorf("creating fleet: %w", err)
	}
	return nil, CreateFleetOutput{Fleet: fleetSummary(created), DryRun: in.DryRun}, nil
}

func buildResourceRequirements(cpuRequest, cpuLimit, memRequest, memLimit string) (corev1.ResourceRequirements, error) {
	req := corev1.ResourceList{}
	lim := corev1.ResourceList{}
	if err := setQuantity(req, corev1.ResourceCPU, cpuRequest); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if err := setQuantity(lim, corev1.ResourceCPU, cpuLimit); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if err := setQuantity(req, corev1.ResourceMemory, memRequest); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if err := setQuantity(lim, corev1.ResourceMemory, memLimit); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	out := corev1.ResourceRequirements{}
	if len(req) > 0 {
		out.Requests = req
	}
	if len(lim) > 0 {
		out.Limits = lim
	}
	return out, nil
}

type DeleteFleetInput struct {
	Name      string `json:"name" jsonschema:"Fleet name"`
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific Fleet, so there's no 'all namespaces' option)"`
	Force     bool   `json:"force,omitempty" jsonschema:"Set true only if you intend to disconnect any players on GameServers under this fleet - required if any are Allocated"`
	DryRun    bool   `json:"dryRun,omitempty" jsonschema:"Validate server-side without deleting anything; the response shows what would have happened"`
	Cluster   string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type DeleteFleetOutput struct {
	Deleted   bool   `json:"deleted"`
	Allocated int32  `json:"allocated"`
	DryRun    bool   `json:"dryRun,omitempty" jsonschema:"True: nothing was actually deleted"`
	Warning   string `json:"warning,omitempty"`
}

// Deleting a Fleet cascades to its GameServerSets and GameServers via
// Kubernetes owner references, so this needs the same live-match guard as
// deleteGameServer. Unlike that one, there's no single ResourceVersion that
// can precondition this delete against a new allocation landing on a child
// GameServer between the check and the call - the state we're guarding
// lives on different objects than the one being deleted. This narrows the
// race window as much as a single check reasonably can; it doesn't close it
// the way the ResourceVersion precondition does for deleteGameServer.
func (s *server) deleteFleet(ctx context.Context, req *mcp.CallToolRequest, in DeleteFleetInput) (*mcp.CallToolResult, DeleteFleetOutput, error) {
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, DeleteFleetOutput{}, err
	}

	fleetSelector := fmt.Sprintf("%s=%s", agonesv1.FleetNameLabel, in.Name)
	gameServers, err := listAllGameServers(ctx, cl, in.Namespace, fleetSelector)
	if err != nil {
		return nil, DeleteFleetOutput{}, err
	}
	var allocated int32
	for _, gs := range gameServers {
		if gs.Labels[agonesv1.FleetNameLabel] != in.Name {
			continue
		}
		if gs.Status.State == agonesv1.GameServerStateAllocated {
			allocated++
		}
	}
	if allocated > 0 && !in.Force {
		return nil, DeleteFleetOutput{
			Deleted:   false,
			Allocated: allocated,
			Warning:   fmt.Sprintf("refused: %d GameServer(s) under this fleet are Allocated with live matches; pass force=true to delete anyway", allocated),
		}, nil
	}

	if err := cl.agones.AgonesV1().Fleets(in.Namespace).Delete(ctx, in.Name, metav1.DeleteOptions{DryRun: dryRunOpt(in.DryRun)}); err != nil {
		return nil, DeleteFleetOutput{}, fmt.Errorf("deleting fleet: %w", err)
	}
	warning := ""
	if in.Force && allocated > 0 && !in.DryRun {
		warning = fmt.Sprintf("force-deleted fleet with %d Allocated GameServer(s); those players are being disconnected", allocated)
	}
	return nil, DeleteFleetOutput{Deleted: true, Allocated: allocated, DryRun: in.DryRun, Warning: warning}, nil
}
