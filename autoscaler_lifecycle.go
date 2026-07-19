package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
)

// parseBufferSize accepts an absolute positive count (e.g. "5") or a
// percentage 1-99 (e.g. "20%"), matching what Agones's own admission webhook
// accepts (see BufferPolicy.ValidateBufferPolicy in
// agones.dev/agones/pkg/apis/autoscaling/v1/fleetautoscaler.go). 0% and 100%
// are rejected by Agones itself: a fleet at either extreme could never scale
// back away from it.
func parseBufferSize(s string) (intstr.IntOrString, error) {
	if s == "" {
		return intstr.IntOrString{}, fmt.Errorf("bufferSize is required")
	}
	// Parsed by hand rather than via intstr.Parse: that helper silently
	// truncates out-of-int32-range numbers, turning "4294967297" into 1.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 || n > maxScaleFleetReplicas {
			return intstr.IntOrString{}, fmt.Errorf("bufferSize must be between 1 and %d, got %q", maxScaleFleetReplicas, s)
		}
		return intstr.FromInt32(int32(n)), nil
	}
	if !strings.HasSuffix(s, "%") {
		return intstr.IntOrString{}, fmt.Errorf("bufferSize must be an absolute number or a percentage like \"20%%\", got %q", s)
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
	if err != nil || n < 1 || n > 99 {
		return intstr.IntOrString{}, fmt.Errorf("bufferSize percentage must be between 1%% and 99%%, got %q", s)
	}
	return intstr.FromString(s), nil
}

// validateBufferBounds mirrors Agones's own admission-webhook validation
// (BufferPolicy.ValidateBufferPolicy) so an invalid combination is rejected
// here with a clear message instead of surfacing as a raw webhook error.
func validateBufferBounds(bufferSize intstr.IntOrString, minReplicas, maxReplicas int32) error {
	if maxReplicas <= 0 || maxReplicas > maxScaleFleetReplicas {
		return fmt.Errorf("maxReplicas must be > 0 and <= %d, got %d", maxScaleFleetReplicas, maxReplicas)
	}
	if minReplicas < 0 {
		return fmt.Errorf("minReplicas must be >= 0, got %d", minReplicas)
	}
	if minReplicas > maxReplicas {
		return fmt.Errorf("minReplicas (%d) must be <= maxReplicas (%d)", minReplicas, maxReplicas)
	}
	if bufferSize.Type == intstr.Int {
		bs := int32(bufferSize.IntValue())
		if maxReplicas < bs {
			return fmt.Errorf("maxReplicas (%d) must be >= bufferSize (%d)", maxReplicas, bs)
		}
		if minReplicas != 0 && minReplicas < bs {
			return fmt.Errorf("minReplicas (%d) must be 0 or >= bufferSize (%d)", minReplicas, bs)
		}
		return nil
	}
	if minReplicas < 1 {
		return fmt.Errorf("minReplicas must be >= 1 when bufferSize is a percentage: with 0 minReplicas and 0 Allocated, the fleet could never scale back above zero")
	}
	return nil
}

type CreateAutoscalerInput struct {
	Name        string `json:"name" jsonschema:"FleetAutoscaler name"`
	Namespace   string `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific FleetAutoscaler, so there's no 'all namespaces' option)"`
	Fleet       string `json:"fleet" jsonschema:"Name of the Fleet this autoscaler controls"`
	BufferSize  string `json:"bufferSize" jsonschema:"Target Ready buffer the autoscaler maintains: an absolute count > 0 (e.g. '5') or a percentage 1%-99% of desired replicas (e.g. '20%')"`
	MinReplicas int32  `json:"minReplicas,omitempty" jsonschema:"Minimum replica floor. For an absolute bufferSize, must be 0 (no minimum) or >= bufferSize. For a percentage bufferSize, must be >= 1 (Agones can't guarantee a percentage buffer with no minimum)"`
	MaxReplicas int32  `json:"maxReplicas" jsonschema:"Maximum replica ceiling; must be > 0, >= minReplicas, and >= bufferSize when bufferSize is absolute"`
	Cluster     string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type CreateAutoscalerOutput struct {
	Autoscaler AutoscalerSummary `json:"autoscaler"`
}

// This tool only builds Buffer-policy autoscalers. Counter/List/Webhook/Schedule/Chain/Wasm
// policies are feature-gated or depend on infrastructure this server doesn't manage (an
// external webhook, a WASM module), so they're out of scope here.
func (s *server) createAutoscaler(ctx context.Context, req *mcp.CallToolRequest, in CreateAutoscalerInput) (*mcp.CallToolResult, CreateAutoscalerOutput, error) {
	if in.Fleet == "" {
		return nil, CreateAutoscalerOutput{}, fmt.Errorf("fleet is required")
	}
	bufferSize, err := parseBufferSize(in.BufferSize)
	if err != nil {
		return nil, CreateAutoscalerOutput{}, err
	}
	if err := validateBufferBounds(bufferSize, in.MinReplicas, in.MaxReplicas); err != nil {
		return nil, CreateAutoscalerOutput{}, err
	}

	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, CreateAutoscalerOutput{}, err
	}

	fa := &autoscalingv1.FleetAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace},
		Spec: autoscalingv1.FleetAutoscalerSpec{
			FleetName: in.Fleet,
			Policy: autoscalingv1.FleetAutoscalerPolicy{
				Type: autoscalingv1.BufferPolicyType,
				Buffer: &autoscalingv1.BufferPolicy{
					BufferSize:  bufferSize,
					MinReplicas: in.MinReplicas,
					MaxReplicas: in.MaxReplicas,
				},
			},
		},
	}

	created, err := cl.agones.AutoscalingV1().FleetAutoscalers(in.Namespace).Create(ctx, fa, metav1.CreateOptions{})
	if err != nil {
		return nil, CreateAutoscalerOutput{}, fmt.Errorf("creating autoscaler: %w", err)
	}
	return nil, CreateAutoscalerOutput{Autoscaler: autoscalerSummary(created)}, nil
}

type UpdateAutoscalerInput struct {
	Name        string  `json:"name" jsonschema:"FleetAutoscaler name"`
	Namespace   string  `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific FleetAutoscaler, so there's no 'all namespaces' option)"`
	BufferSize  *string `json:"bufferSize,omitempty" jsonschema:"New buffer size, absolute count or percentage like '20%'; omit to leave unchanged"`
	MinReplicas *int32  `json:"minReplicas,omitempty" jsonschema:"New minimum replica floor; omit to leave unchanged (0 is a valid explicit value meaning no minimum)"`
	MaxReplicas *int32  `json:"maxReplicas,omitempty" jsonschema:"New maximum replica ceiling; omit to leave unchanged"`
	Cluster     string  `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type UpdateAutoscalerOutput struct {
	Autoscaler AutoscalerSummary `json:"autoscaler"`
}

func (s *server) updateAutoscaler(ctx context.Context, req *mcp.CallToolRequest, in UpdateAutoscalerInput) (*mcp.CallToolResult, UpdateAutoscalerOutput, error) {
	if in.BufferSize == nil && in.MinReplicas == nil && in.MaxReplicas == nil {
		return nil, UpdateAutoscalerOutput{}, fmt.Errorf("at least one of bufferSize, minReplicas, maxReplicas must be provided")
	}
	var newBuffer intstr.IntOrString
	if in.BufferSize != nil {
		v, err := parseBufferSize(*in.BufferSize)
		if err != nil {
			return nil, UpdateAutoscalerOutput{}, err
		}
		newBuffer = v
	}

	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, UpdateAutoscalerOutput{}, err
	}

	var out AutoscalerSummary
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fa, err := cl.agones.AutoscalingV1().FleetAutoscalers(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if fa.Spec.Policy.Buffer == nil {
			return fmt.Errorf("autoscaler %q is a %s-policy autoscaler; this tool only updates Buffer-policy autoscalers", in.Name, fa.Spec.Policy.Type)
		}
		if in.BufferSize != nil {
			fa.Spec.Policy.Buffer.BufferSize = newBuffer
		}
		if in.MinReplicas != nil {
			fa.Spec.Policy.Buffer.MinReplicas = *in.MinReplicas
		}
		if in.MaxReplicas != nil {
			fa.Spec.Policy.Buffer.MaxReplicas = *in.MaxReplicas
		}
		if err := validateBufferBounds(fa.Spec.Policy.Buffer.BufferSize, fa.Spec.Policy.Buffer.MinReplicas, fa.Spec.Policy.Buffer.MaxReplicas); err != nil {
			return err
		}
		updated, err := cl.agones.AutoscalingV1().FleetAutoscalers(in.Namespace).Update(ctx, fa, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		out = autoscalerSummary(updated)
		return nil
	})
	if err != nil {
		return nil, UpdateAutoscalerOutput{}, err
	}
	return nil, UpdateAutoscalerOutput{Autoscaler: out}, nil
}

type DeleteAutoscalerInput struct {
	Name      string `json:"name" jsonschema:"FleetAutoscaler name"`
	Namespace string `json:"namespace" jsonschema:"Kubernetes namespace (required: this tool targets one specific FleetAutoscaler, so there's no 'all namespaces' option)"`
	Cluster   string `json:"cluster,omitempty" jsonschema:"Cluster to target; omit for the default cluster"`
}

type DeleteAutoscalerOutput struct {
	Deleted bool `json:"deleted"`
}

// Deleting a FleetAutoscaler only removes the autoscaler object itself; the
// Fleet and its GameServers are untouched and simply stop being auto-scaled.
func (s *server) deleteAutoscaler(ctx context.Context, req *mcp.CallToolRequest, in DeleteAutoscalerInput) (*mcp.CallToolResult, DeleteAutoscalerOutput, error) {
	cl, err := s.c.get(in.Cluster)
	if err != nil {
		return nil, DeleteAutoscalerOutput{}, err
	}
	if err := cl.agones.AutoscalingV1().FleetAutoscalers(in.Namespace).Delete(ctx, in.Name, metav1.DeleteOptions{}); err != nil {
		return nil, DeleteAutoscalerOutput{}, fmt.Errorf("deleting autoscaler: %w", err)
	}
	return nil, DeleteAutoscalerOutput{Deleted: true}, nil
}
