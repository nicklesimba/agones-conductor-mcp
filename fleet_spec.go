package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

func setQuantity(list corev1.ResourceList, name corev1.ResourceName, val string) error {
	if val == "" {
		return nil
	}
	q, err := resource.ParseQuantity(val)
	if err != nil {
		return fmt.Errorf("invalid %s quantity %q: %w", name, val, err)
	}
	list[name] = q
	return nil
}

// applyResourceOverrides starts from a container's existing resource
// requests/limits and replaces only the ones a non-empty string was given
// for, so a caller can adjust just CPU or just memory without having to
// restate the other's current value.
func applyResourceOverrides(existing corev1.ResourceRequirements, cpuRequest, cpuLimit, memRequest, memLimit string) (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{
		Requests: existing.Requests.DeepCopy(),
		Limits:   existing.Limits.DeepCopy(),
	}
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	if out.Limits == nil {
		out.Limits = corev1.ResourceList{}
	}
	if err := setQuantity(out.Requests, corev1.ResourceCPU, cpuRequest); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if err := setQuantity(out.Limits, corev1.ResourceCPU, cpuLimit); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if err := setQuantity(out.Requests, corev1.ResourceMemory, memRequest); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if err := setQuantity(out.Limits, corev1.ResourceMemory, memLimit); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if len(out.Requests) == 0 {
		out.Requests = nil
	}
	if len(out.Limits) == 0 {
		out.Limits = nil
	}
	return out, nil
}

type UpdateFleetResourcesInput struct {
	Fleet         string `json:"fleet" jsonschema:"Fleet name"`
	Namespace     string `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific Fleet, so there's no 'all namespaces' option)"`
	Container     string `json:"container,omitempty" jsonschema:"Container name to update; required only if the GameServer template defines more than one container"`
	CPURequest    string `json:"cpuRequest,omitempty" jsonschema:"e.g. 100m; omit to leave the current CPU request unchanged"`
	CPULimit      string `json:"cpuLimit,omitempty" jsonschema:"e.g. 500m; omit to leave the current CPU limit unchanged"`
	MemoryRequest string `json:"memoryRequest,omitempty" jsonschema:"e.g. 128Mi; omit to leave the current memory request unchanged"`
	MemoryLimit   string `json:"memoryLimit,omitempty" jsonschema:"e.g. 256Mi; omit to leave the current memory limit unchanged"`
	DryRun        bool   `json:"dryRun,omitempty" jsonschema:"Validate server-side without persisting anything"`
	Cluster       string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type ResourceSummary struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

type UpdateFleetResourcesOutput struct {
	Fleet     string          `json:"fleet"`
	Container string          `json:"container"`
	Resources ResourceSummary `json:"resources"`
	DryRun    bool            `json:"dryRun,omitempty" jsonschema:"True: nothing was actually changed"`
}

func resourceSummary(r corev1.ResourceRequirements) ResourceSummary {
	var out ResourceSummary
	if r.Requests != nil {
		if q, ok := r.Requests[corev1.ResourceCPU]; ok {
			out.CPURequest = q.String()
		}
		if q, ok := r.Requests[corev1.ResourceMemory]; ok {
			out.MemoryRequest = q.String()
		}
	}
	if r.Limits != nil {
		if q, ok := r.Limits[corev1.ResourceCPU]; ok {
			out.CPULimit = q.String()
		}
		if q, ok := r.Limits[corev1.ResourceMemory]; ok {
			out.MemoryLimit = q.String()
		}
	}
	return out
}

// Patches CPU/memory requests and limits on one container of the fleet's
// GameServer template and lets Agones's own rolling update roll the change
// out; use rollout_status to track progress, same as update_fleet_image.
func (s *server) updateFleetResources(ctx context.Context, req *mcp.CallToolRequest, in UpdateFleetResourcesInput) (*mcp.CallToolResult, UpdateFleetResourcesOutput, error) {
	if in.CPURequest == "" && in.CPULimit == "" && in.MemoryRequest == "" && in.MemoryLimit == "" {
		return nil, UpdateFleetResourcesOutput{}, fmt.Errorf("at least one of cpuRequest, cpuLimit, memoryRequest, memoryLimit must be provided")
	}
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, UpdateFleetResourcesOutput{}, err
	}

	var out UpdateFleetResourcesOutput
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
		resources, err := applyResourceOverrides(containers[idx].Resources, in.CPURequest, in.CPULimit, in.MemoryRequest, in.MemoryLimit)
		if err != nil {
			return err
		}
		fleet.Spec.Template.Spec.Template.Spec.Containers[idx].Resources = resources
		updated, err := cl.agones.AgonesV1().Fleets(in.Namespace).Update(ctx, fleet, metav1.UpdateOptions{DryRun: dryRunOpt(in.DryRun)})
		if err != nil {
			return err
		}
		out = UpdateFleetResourcesOutput{
			Fleet:     in.Fleet,
			Container: containers[idx].Name,
			Resources: resourceSummary(updated.Spec.Template.Spec.Template.Spec.Containers[idx].Resources),
			DryRun:    in.DryRun,
		}
		return nil
	})
	if err != nil {
		return nil, UpdateFleetResourcesOutput{}, err
	}
	return nil, out, nil
}

type UpdateFleetHealthInput struct {
	Fleet               string `json:"fleet" jsonschema:"Fleet name"`
	Namespace           string `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific Fleet, so there's no 'all namespaces' option)"`
	Disabled            *bool  `json:"disabled,omitempty" jsonschema:"Turn Agones health checking off (true) or on (false) for this fleet's GameServers; omit to leave unchanged"`
	PeriodSeconds       int32  `json:"periodSeconds,omitempty" jsonschema:"Seconds between health pings; omit (0) to leave unchanged. Agones' own default is 5"`
	FailureThreshold    int32  `json:"failureThreshold,omitempty" jsonschema:"Consecutive failed pings before a GameServer is marked Unhealthy; omit (0) to leave unchanged. Agones' own default is 3"`
	InitialDelaySeconds int32  `json:"initialDelaySeconds,omitempty" jsonschema:"Seconds to wait after start before the first health check; omit (0) to leave unchanged. Agones' own default is 5"`
	DryRun              bool   `json:"dryRun,omitempty" jsonschema:"Validate server-side without persisting anything"`
	Cluster             string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type HealthSummary struct {
	Disabled            bool  `json:"disabled"`
	PeriodSeconds       int32 `json:"periodSeconds"`
	FailureThreshold    int32 `json:"failureThreshold"`
	InitialDelaySeconds int32 `json:"initialDelaySeconds"`
}

type UpdateFleetHealthOutput struct {
	Fleet  string        `json:"fleet"`
	Health HealthSummary `json:"health"`
	DryRun bool          `json:"dryRun,omitempty" jsonschema:"True: nothing was actually changed"`
}

type UpdateFleetEnvInput struct {
	Fleet     string            `json:"fleet" jsonschema:"Fleet name"`
	Namespace string            `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific Fleet, so there's no 'all namespaces' option)"`
	Container string            `json:"container,omitempty" jsonschema:"Container name to update; required only if the GameServer template defines more than one container"`
	Set       map[string]string `json:"set,omitempty" jsonschema:"Environment variables to set or overwrite, keyed by name"`
	Unset     []string          `json:"unset,omitempty" jsonschema:"Environment variable names to remove; names not present are ignored"`
	DryRun    bool              `json:"dryRun,omitempty" jsonschema:"Validate server-side without persisting anything"`
	Cluster   string            `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type UpdateFleetEnvOutput struct {
	Fleet     string   `json:"fleet"`
	Container string   `json:"container"`
	Env       []string `json:"env" jsonschema:"The container's resulting environment, as NAME=value (NAME=(from source) for secret/configmap references)"`
	DryRun    bool     `json:"dryRun,omitempty" jsonschema:"True: nothing was actually changed"`
}

func applyEnvOverrides(existing []corev1.EnvVar, set map[string]string, unset []string) []corev1.EnvVar {
	drop := make(map[string]bool, len(unset))
	for _, name := range unset {
		drop[name] = true
	}
	remaining := make(map[string]string, len(set))
	for k, v := range set {
		remaining[k] = v
	}
	out := make([]corev1.EnvVar, 0, len(existing)+len(set))
	for _, e := range existing {
		if drop[e.Name] {
			continue
		}
		if v, ok := remaining[e.Name]; ok {
			out = append(out, corev1.EnvVar{Name: e.Name, Value: v})
			delete(remaining, e.Name)
			continue
		}
		out = append(out, e)
	}
	// Sorted so repeat calls produce identical specs instead of reshuffled
	// ones that would trigger a pointless rollout.
	names := make([]string, 0, len(remaining))
	for k := range remaining {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		out = append(out, corev1.EnvVar{Name: k, Value: remaining[k]})
	}
	return out
}

func envStrings(env []corev1.EnvVar) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if e.ValueFrom != nil {
			out = append(out, e.Name+"=(from source)")
			continue
		}
		out = append(out, e.Name+"="+e.Value)
	}
	return out
}

// Config-via-env is the most common reason to roll a fleet; this patches one
// container's env and lets Agones's allocation-aware rolling update do the
// rest, same as update_fleet_image.
func (s *server) updateFleetEnv(ctx context.Context, req *mcp.CallToolRequest, in UpdateFleetEnvInput) (*mcp.CallToolResult, UpdateFleetEnvOutput, error) {
	if len(in.Set) == 0 && len(in.Unset) == 0 {
		return nil, UpdateFleetEnvOutput{}, fmt.Errorf("at least one of set, unset must be provided")
	}
	for k := range in.Set {
		if k == "" {
			return nil, UpdateFleetEnvOutput{}, fmt.Errorf("set: empty variable name")
		}
	}
	for _, k := range in.Unset {
		if k == "" {
			return nil, UpdateFleetEnvOutput{}, fmt.Errorf("unset: empty variable name")
		}
		if _, both := in.Set[k]; both {
			return nil, UpdateFleetEnvOutput{}, fmt.Errorf("%q appears in both set and unset", k)
		}
	}
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, UpdateFleetEnvOutput{}, err
	}

	var out UpdateFleetEnvOutput
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
		fleet.Spec.Template.Spec.Template.Spec.Containers[idx].Env = applyEnvOverrides(containers[idx].Env, in.Set, in.Unset)
		updated, err := cl.agones.AgonesV1().Fleets(in.Namespace).Update(ctx, fleet, metav1.UpdateOptions{DryRun: dryRunOpt(in.DryRun)})
		if err != nil {
			return err
		}
		out = UpdateFleetEnvOutput{
			Fleet:     in.Fleet,
			Container: containers[idx].Name,
			Env:       envStrings(updated.Spec.Template.Spec.Template.Spec.Containers[idx].Env),
			DryRun:    in.DryRun,
		}
		return nil
	})
	if err != nil {
		return nil, UpdateFleetEnvOutput{}, err
	}
	return nil, out, nil
}

// Health only has four fields and none of Agones's own zero values are
// meaningful settings a caller would intentionally choose over the applied
// default (0 seconds between pings, 0 failures tolerated), so, like
// UpdateAutoscalerInput.MinReplicas, 0/omitted means "leave unchanged" here
// - except Disabled, where false is a legitimate explicit value, hence the
// pointer.
func (s *server) updateFleetHealth(ctx context.Context, req *mcp.CallToolRequest, in UpdateFleetHealthInput) (*mcp.CallToolResult, UpdateFleetHealthOutput, error) {
	if in.Disabled == nil && in.PeriodSeconds == 0 && in.FailureThreshold == 0 && in.InitialDelaySeconds == 0 {
		return nil, UpdateFleetHealthOutput{}, fmt.Errorf("at least one of disabled, periodSeconds, failureThreshold, initialDelaySeconds must be provided")
	}
	if in.PeriodSeconds < 0 || in.FailureThreshold < 0 || in.InitialDelaySeconds < 0 {
		return nil, UpdateFleetHealthOutput{}, fmt.Errorf("periodSeconds, failureThreshold, and initialDelaySeconds must be >= 0")
	}
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, UpdateFleetHealthOutput{}, err
	}

	var out UpdateFleetHealthOutput
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fleet, err := cl.agones.AgonesV1().Fleets(in.Namespace).Get(ctx, in.Fleet, metav1.GetOptions{})
		if err != nil {
			return err
		}
		h := &fleet.Spec.Template.Spec.Health
		if in.Disabled != nil {
			h.Disabled = *in.Disabled
		}
		if in.PeriodSeconds != 0 {
			h.PeriodSeconds = in.PeriodSeconds
		}
		if in.FailureThreshold != 0 {
			h.FailureThreshold = in.FailureThreshold
		}
		if in.InitialDelaySeconds != 0 {
			h.InitialDelaySeconds = in.InitialDelaySeconds
		}
		updated, err := cl.agones.AgonesV1().Fleets(in.Namespace).Update(ctx, fleet, metav1.UpdateOptions{DryRun: dryRunOpt(in.DryRun)})
		if err != nil {
			return err
		}
		uh := updated.Spec.Template.Spec.Health
		out = UpdateFleetHealthOutput{
			Fleet: in.Fleet,
			Health: HealthSummary{
				Disabled:            uh.Disabled,
				PeriodSeconds:       uh.PeriodSeconds,
				FailureThreshold:    uh.FailureThreshold,
				InitialDelaySeconds: uh.InitialDelaySeconds,
			},
			DryRun: in.DryRun,
		}
		return nil
	})
	if err != nil {
		return nil, UpdateFleetHealthOutput{}, err
	}
	return nil, out, nil
}
