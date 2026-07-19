package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

type UpdateFleetImageInput struct {
	Fleet     string `json:"fleet" jsonschema:"Fleet name"`
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific Fleet, so there's no 'all namespaces' option)"`
	Image     string `json:"image" jsonschema:"New container image, e.g. gcr.io/my-project/my-game:v2"`
	Container string `json:"container,omitempty" jsonschema:"Container name to update; required only if the GameServer template defines more than one container"`
	Cluster   string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type UpdateFleetImageOutput struct {
	Fleet         string `json:"fleet"`
	Container     string `json:"container"`
	PreviousImage string `json:"previousImage"`
	NewImage      string `json:"newImage"`
}

// Patches the image and lets Agones's own rolling update handle the rest;
// use rolloutStatus to track progress.
func (s *server) updateFleetImage(ctx context.Context, req *mcp.CallToolRequest, in UpdateFleetImageInput) (*mcp.CallToolResult, UpdateFleetImageOutput, error) {
	if strings.TrimSpace(in.Image) == "" {
		return nil, UpdateFleetImageOutput{}, fmt.Errorf("image is required")
	}
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, UpdateFleetImageOutput{}, err
	}
	var out UpdateFleetImageOutput
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fleet, err := cl.agones.AgonesV1().Fleets(in.Namespace).Get(ctx, in.Fleet, metav1.GetOptions{})
		if err != nil {
			return err
		}
		containers := fleet.Spec.Template.Spec.Template.Spec.Containers
		idx, err := selectContainer(containers, in.Container)
		if err != nil {
			return err
		}
		previous := containers[idx].Image
		fleet.Spec.Template.Spec.Template.Spec.Containers[idx].Image = in.Image
		updated, err := cl.agones.AgonesV1().Fleets(in.Namespace).Update(ctx, fleet, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		// Like the other update tools, report what the server persisted.
		out = UpdateFleetImageOutput{
			Fleet:         in.Fleet,
			Container:     containers[idx].Name,
			PreviousImage: previous,
			NewImage:      updated.Spec.Template.Spec.Template.Spec.Containers[idx].Image,
		}
		return nil
	})
	if err != nil {
		return nil, UpdateFleetImageOutput{}, err
	}
	return nil, out, nil
}

func selectContainer(containers []corev1.Container, name string) (int, error) {
	if name != "" {
		for i, c := range containers {
			if c.Name == name {
				return i, nil
			}
		}
		return 0, fmt.Errorf("container %q not found in fleet template", name)
	}
	if len(containers) == 1 {
		return 0, nil
	}
	return 0, fmt.Errorf("fleet template defines %d containers; specify which one with the container field", len(containers))
}

type RolloutVersion struct {
	GameServerSet string `json:"gameServerSet"`
	Image         string `json:"image,omitempty"`
	Replicas      int32  `json:"replicas"`
	Ready         int32  `json:"ready"`
	Allocated     int32  `json:"allocated"`
}

type RolloutStatusOutput struct {
	Fleet           string           `json:"fleet"`
	Namespace       string           `json:"namespace"`
	DesiredReplicas int32            `json:"desiredReplicas"`
	Current         RolloutVersion   `json:"current"`
	Previous        []RolloutVersion `json:"previous,omitempty"`
	PercentComplete float64          `json:"percentComplete" jsonschema:"Share of desired replicas existing on the current template version, regardless of readiness; can read 100 while complete is still false (old servers draining or new ones still booting)"`
	Complete        bool             `json:"complete" jsonschema:"True only when every previous-version server is gone AND every current-version replica is Ready or Allocated"`
	Warnings        []string         `json:"warnings,omitempty"`
}

// Counts live GameServer state per GameServerSet rather than trusting
// GameServerSet.Status, which lags for the same reason Fleet.Status does.
func (s *server) rolloutStatus(ctx context.Context, req *mcp.CallToolRequest, in NamedInput) (*mcp.CallToolResult, RolloutStatusOutput, error) {
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, RolloutStatusOutput{}, err
	}
	fleet, err := cl.agones.AgonesV1().Fleets(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return nil, RolloutStatusOutput{}, err
	}
	fleetSelector := fmt.Sprintf("%s=%s", agonesv1.FleetNameLabel, in.Name)
	setList, err := cl.agones.AgonesV1().GameServerSets(in.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fleetSelector,
	})
	if err != nil {
		return nil, RolloutStatusOutput{}, err
	}
	gameServers, err := listAllGameServers(ctx, cl, in.Namespace, fleetSelector)
	if err != nil {
		return nil, RolloutStatusOutput{}, err
	}

	type counts struct{ replicas, ready, allocated int32 }
	live := map[string]*counts{}
	for _, gs := range gameServers {
		if gs.Labels[agonesv1.FleetNameLabel] != in.Name {
			continue
		}
		setName := gs.Labels[agonesv1.GameServerSetGameServerLabel]
		c, ok := live[setName]
		if !ok {
			c = &counts{}
			live[setName] = c
		}
		c.replicas++
		switch gs.Status.State {
		case agonesv1.GameServerStateReady:
			c.ready++
		case agonesv1.GameServerStateAllocated:
			c.allocated++
		}
	}

	out := RolloutStatusOutput{
		Fleet:           in.Name,
		Namespace:       in.Namespace,
		DesiredReplicas: fleet.Spec.Replicas,
	}

	// Usually exactly one set matches; more than one can happen briefly on a
	// rollback, so counts get summed instead of picking just one.
	var currentSets []RolloutVersion
	knownSets := map[string]bool{}
	for _, gss := range setList.Items {
		if gss.Labels[agonesv1.FleetNameLabel] != in.Name {
			continue
		}
		knownSets[gss.Name] = true
		c := live[gss.Name]
		if c == nil {
			c = &counts{}
		}
		v := RolloutVersion{
			GameServerSet: gss.Name,
			Image:         gameServerContainerImage(gss.Spec.Template.Spec),
			Replicas:      c.replicas,
			Ready:         c.ready,
			Allocated:     c.allocated,
		}
		if apiequality.Semantic.DeepEqual(gss.Spec.Template, fleet.Spec.Template) {
			currentSets = append(currentSets, v)
		} else if c.replicas > 0 {
			out.Previous = append(out.Previous, v)
		}
	}

	switch len(currentSets) {
	case 0:
		// out.Current stays zero-value; the warning below explains why.
	case 1:
		out.Current = currentSets[0]
	default:
		names := make([]string, len(currentSets))
		for i, v := range currentSets {
			names[i] = v.GameServerSet
			out.Current.Replicas += v.Replicas
			out.Current.Ready += v.Ready
			out.Current.Allocated += v.Allocated
		}
		out.Current.GameServerSet = strings.Join(names, ",")
		out.Current.Image = currentSets[0].Image
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"multiple GameServerSets match the current fleet template (%s); their counts have been combined",
			strings.Join(names, ", ")))
	}

	if out.DesiredReplicas > 0 {
		out.PercentComplete = float64(out.Current.Replicas) / float64(out.DesiredReplicas) * 100
		if out.PercentComplete > 100 {
			out.PercentComplete = 100
		}
	} else {
		out.PercentComplete = 100
	}
	// "Complete" means what it means everywhere else in Kubernetes: the new
	// version isn't just scheduled, it's actually up.
	currentUp := out.Current.Ready + out.Current.Allocated
	out.Complete = len(out.Previous) == 0 && out.Current.Replicas == out.DesiredReplicas && currentUp == out.Current.Replicas
	if len(out.Previous) == 0 && out.Current.Replicas == out.DesiredReplicas && currentUp < out.Current.Replicas {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%d current-version GameServer(s) exist but are not yet Ready or Allocated", out.Current.Replicas-currentUp))
	}

	if out.Current.GameServerSet == "" && out.DesiredReplicas > 0 {
		out.Warnings = append(out.Warnings, "no GameServerSet currently matches the fleet's spec; a rollout may still be initializing")
	}
	for _, p := range out.Previous {
		if p.Allocated > 0 {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"%s (previous version) still has %d GameServer(s) with live matches; they will not be forcibly disrupted and will drain naturally as those matches end",
				p.GameServerSet, p.Allocated))
		}
	}
	if orphaned := live[""]; orphaned != nil && orphaned.replicas > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%d GameServer(s) for this fleet have no owning GameServerSet label and were excluded from the counts above",
			orphaned.replicas))
	}
	// A GameServerSet created between the two List calls above would have
	// GameServers in live but no entry in setList - surface those too.
	var strayNames []string
	for setName, c := range live {
		if setName == "" || knownSets[setName] || c.replicas == 0 {
			continue
		}
		strayNames = append(strayNames, setName)
	}
	sort.Strings(strayNames)
	for _, setName := range strayNames {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%d GameServer(s) reference GameServerSet %q, which wasn't found in this lookup (it may have just been created or deleted); excluded from the counts above",
			live[setName].replicas, setName))
	}

	return nil, out, nil
}

// Unlike selectContainer, a named-but-missing container falls through to the
// heuristics below: this feeds a display field, so a best guess beats an
// error.
func gameServerContainerImage(spec agonesv1.GameServerSpec) string {
	containers := spec.Template.Spec.Containers
	if spec.Container != "" {
		for _, c := range containers {
			if c.Name == spec.Container {
				return c.Image
			}
		}
	}
	if len(containers) == 1 {
		return containers[0].Image
	}
	images := make([]string, 0, len(containers))
	for _, c := range containers {
		images = append(images, c.Image)
	}
	return strings.Join(images, ",")
}
