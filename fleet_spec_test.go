package main

import (
	"context"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUpdateFleetResources_SetsBothRequestAndLimit(t *testing.T) {
	fleet := testFleetWithTemplate("res-fleet", "default", 2, corev1.Container{Name: "game", Image: "example/game:v1"})
	s := newTestServer(fleet)

	_, out, err := s.updateFleetResources(context.Background(), nil, UpdateFleetResourcesInput{
		Fleet: "res-fleet", Namespace: "default", CPURequest: "100m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "256Mi",
	})
	if err != nil {
		t.Fatalf("updateFleetResources: %v", err)
	}
	want := ResourceSummary{CPURequest: "100m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "256Mi"}
	if out.Resources != want {
		t.Fatalf("Resources = %+v, want %+v", out.Resources, want)
	}

	updated, err := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "res-fleet", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	res := updated.Spec.Template.Spec.Template.Spec.Containers[0].Resources
	if res.Requests.Cpu().String() != "100m" || res.Limits.Memory().String() != "256Mi" {
		t.Fatalf("unexpected persisted resources: %+v", res)
	}
}

func TestUpdateFleetResources_PartialUpdateLeavesOtherFieldsUnchanged(t *testing.T) {
	existing := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	fleet := testFleetWithTemplate("res-fleet", "default", 2, corev1.Container{Name: "game", Image: "example/game:v1", Resources: existing})
	s := newTestServer(fleet)

	_, out, err := s.updateFleetResources(context.Background(), nil, UpdateFleetResourcesInput{
		Fleet: "res-fleet", Namespace: "default", MemoryLimit: "512Mi",
	})
	if err != nil {
		t.Fatalf("updateFleetResources: %v", err)
	}
	want := ResourceSummary{CPURequest: "100m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "512Mi"}
	if out.Resources != want {
		t.Fatalf("Resources = %+v, want %+v (only memoryLimit should have changed)", out.Resources, want)
	}
}

func TestUpdateFleetResources_RejectsInvalidQuantity(t *testing.T) {
	fleet := testFleetWithTemplate("res-fleet", "default", 2, corev1.Container{Name: "game", Image: "example/game:v1"})
	s := newTestServer(fleet)

	_, _, err := s.updateFleetResources(context.Background(), nil, UpdateFleetResourcesInput{
		Fleet: "res-fleet", Namespace: "default", CPURequest: "not-a-quantity",
	})
	if err == nil {
		t.Fatal("expected an error for an invalid CPU quantity, got nil")
	}
}

func TestUpdateFleetResources_RejectsNoFieldsProvided(t *testing.T) {
	fleet := testFleetWithTemplate("res-fleet", "default", 2, corev1.Container{Name: "game", Image: "example/game:v1"})
	s := newTestServer(fleet)

	_, _, err := s.updateFleetResources(context.Background(), nil, UpdateFleetResourcesInput{Fleet: "res-fleet", Namespace: "default"})
	if err == nil {
		t.Fatal("expected an error when no fields are provided, got nil")
	}
}

func TestUpdateFleetResources_AmbiguousContainerWithoutNameErrors(t *testing.T) {
	fleet := testFleetWithTemplate("res-fleet", "default", 2,
		corev1.Container{Name: "game", Image: "example/game:v1"},
		corev1.Container{Name: "sidecar", Image: "example/sidecar:v1"},
	)
	s := newTestServer(fleet)

	_, _, err := s.updateFleetResources(context.Background(), nil, UpdateFleetResourcesInput{
		Fleet: "res-fleet", Namespace: "default", CPURequest: "100m",
	})
	if err == nil {
		t.Fatal("expected an error for an ambiguous multi-container fleet without a container name, got nil")
	}
}

func TestUpdateFleetResources_NonexistentFleetReturnsError(t *testing.T) {
	s := newTestServer()
	_, _, err := s.updateFleetResources(context.Background(), nil, UpdateFleetResourcesInput{
		Fleet: "no-such-fleet", Namespace: "default", CPURequest: "100m",
	})
	if err == nil {
		t.Fatal("expected an error updating a nonexistent fleet, got nil")
	}
}

func TestUpdateFleetHealth_SetsAllFields(t *testing.T) {
	fleet := testFleetWithTemplate("health-fleet", "default", 2, corev1.Container{Name: "game", Image: "example/game:v1"})
	s := newTestServer(fleet)

	_, out, err := s.updateFleetHealth(context.Background(), nil, UpdateFleetHealthInput{
		Fleet: "health-fleet", Namespace: "default",
		PeriodSeconds: 10, FailureThreshold: 5, InitialDelaySeconds: 15,
	})
	if err != nil {
		t.Fatalf("updateFleetHealth: %v", err)
	}
	want := HealthSummary{Disabled: false, PeriodSeconds: 10, FailureThreshold: 5, InitialDelaySeconds: 15}
	if out.Health != want {
		t.Fatalf("Health = %+v, want %+v", out.Health, want)
	}
}

func TestUpdateFleetHealth_ExplicitFalseDisabledIsRespected(t *testing.T) {
	fleet := testFleetWithTemplate("health-fleet", "default", 2, corev1.Container{Name: "game", Image: "example/game:v1"})
	fleet.Spec.Template.Spec.Health = agonesv1.Health{Disabled: true, PeriodSeconds: 5, FailureThreshold: 3, InitialDelaySeconds: 5}
	s := newTestServer(fleet)

	falseVal := false
	_, out, err := s.updateFleetHealth(context.Background(), nil, UpdateFleetHealthInput{
		Fleet: "health-fleet", Namespace: "default", Disabled: &falseVal,
	})
	if err != nil {
		t.Fatalf("updateFleetHealth: %v", err)
	}
	if out.Health.Disabled {
		t.Fatal("Disabled = true, want false (an explicit false must not be mistaken for 'not provided')")
	}
	if out.Health.PeriodSeconds != 5 || out.Health.FailureThreshold != 3 {
		t.Fatalf("unrelated fields should be untouched: %+v", out.Health)
	}
}

func TestUpdateFleetHealth_PartialUpdateLeavesOtherFieldsUnchanged(t *testing.T) {
	fleet := testFleetWithTemplate("health-fleet", "default", 2, corev1.Container{Name: "game", Image: "example/game:v1"})
	fleet.Spec.Template.Spec.Health = agonesv1.Health{PeriodSeconds: 5, FailureThreshold: 3, InitialDelaySeconds: 5}
	s := newTestServer(fleet)

	_, out, err := s.updateFleetHealth(context.Background(), nil, UpdateFleetHealthInput{
		Fleet: "health-fleet", Namespace: "default", FailureThreshold: 10,
	})
	if err != nil {
		t.Fatalf("updateFleetHealth: %v", err)
	}
	want := HealthSummary{PeriodSeconds: 5, FailureThreshold: 10, InitialDelaySeconds: 5}
	if out.Health != want {
		t.Fatalf("Health = %+v, want %+v", out.Health, want)
	}
}

func TestUpdateFleetHealth_RejectsNegativeValues(t *testing.T) {
	fleet := testFleetWithTemplate("health-fleet", "default", 2, corev1.Container{Name: "game", Image: "example/game:v1"})
	s := newTestServer(fleet)

	_, _, err := s.updateFleetHealth(context.Background(), nil, UpdateFleetHealthInput{
		Fleet: "health-fleet", Namespace: "default", PeriodSeconds: -1,
	})
	if err == nil {
		t.Fatal("expected an error for a negative periodSeconds, got nil")
	}
}

func TestUpdateFleetHealth_RejectsNoFieldsProvided(t *testing.T) {
	fleet := testFleetWithTemplate("health-fleet", "default", 2, corev1.Container{Name: "game", Image: "example/game:v1"})
	s := newTestServer(fleet)

	_, _, err := s.updateFleetHealth(context.Background(), nil, UpdateFleetHealthInput{Fleet: "health-fleet", Namespace: "default"})
	if err == nil {
		t.Fatal("expected an error when no fields are provided, got nil")
	}
}

func TestUpdateFleetHealth_NonexistentFleetReturnsError(t *testing.T) {
	s := newTestServer()
	falseVal := false
	_, _, err := s.updateFleetHealth(context.Background(), nil, UpdateFleetHealthInput{
		Fleet: "no-such-fleet", Namespace: "default", Disabled: &falseVal,
	})
	if err == nil {
		t.Fatal("expected an error updating a nonexistent fleet, got nil")
	}
}
