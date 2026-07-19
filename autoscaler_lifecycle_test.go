package main

import (
	"context"
	"testing"

	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCreateAutoscaler_CreatesBufferPolicy(t *testing.T) {
	s := newTestServer()

	_, out, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "ranked-as", Namespace: "default", Fleet: "ranked",
		BufferSize: "5", MinReplicas: 5, MaxReplicas: 20,
	})
	if err != nil {
		t.Fatalf("createAutoscaler: %v", err)
	}
	if out.Autoscaler.Name != "ranked-as" || out.Autoscaler.Fleet != "ranked" || out.Autoscaler.BufferSize != "5" {
		t.Fatalf("unexpected output: %+v", out.Autoscaler)
	}

	created, err := testClients(s).agones.AutoscalingV1().FleetAutoscalers("default").Get(context.Background(), "ranked-as", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if created.Spec.Policy.Type != autoscalingv1.BufferPolicyType {
		t.Fatalf("expected Buffer policy type, got %v", created.Spec.Policy.Type)
	}
	if created.Spec.Policy.Buffer.MinReplicas != 5 || created.Spec.Policy.Buffer.MaxReplicas != 20 {
		t.Fatalf("unexpected bounds: %+v", created.Spec.Policy.Buffer)
	}
}

func TestCreateAutoscaler_ZeroMinReplicasAllowedWithAbsoluteBufferSize(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "zero-min-as", Namespace: "default", Fleet: "ranked",
		BufferSize: "5", MaxReplicas: 20,
	})
	if err != nil {
		t.Fatalf("createAutoscaler: %v", err)
	}
}

func TestCreateAutoscaler_AcceptsPercentageBufferSize(t *testing.T) {
	s := newTestServer()
	_, out, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "pct-as", Namespace: "default", Fleet: "ranked",
		BufferSize: "20%", MinReplicas: 1, MaxReplicas: 20,
	})
	if err != nil {
		t.Fatalf("createAutoscaler: %v", err)
	}
	if out.Autoscaler.BufferSize != "20%" {
		t.Fatalf("BufferSize = %q, want 20%%", out.Autoscaler.BufferSize)
	}
}

func TestCreateAutoscaler_RejectsZeroMinReplicasWithPercentageBufferSize(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "bad-pct-min-as", Namespace: "default", Fleet: "ranked",
		BufferSize: "20%", MaxReplicas: 20,
	})
	if err == nil {
		t.Fatal("expected an error: a percentage bufferSize needs minReplicas >= 1 to guarantee the buffer, got nil")
	}
}

func TestCreateAutoscaler_RejectsMinReplicasBelowAbsoluteBufferSize(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "bad-min-buffer-as", Namespace: "default", Fleet: "ranked",
		BufferSize: "5", MinReplicas: 2, MaxReplicas: 20,
	})
	if err == nil {
		t.Fatal("expected an error: nonzero minReplicas below an absolute bufferSize, got nil")
	}
}

func TestCreateAutoscaler_RejectsMaxReplicasBelowBufferSize(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "bad-max-buffer-as", Namespace: "default", Fleet: "ranked",
		BufferSize: "10", MaxReplicas: 5,
	})
	if err == nil {
		t.Fatal("expected an error: maxReplicas below bufferSize, got nil")
	}
}

func TestCreateAutoscaler_RejectsMissingFleet(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "no-fleet-as", Namespace: "default", BufferSize: "5", MaxReplicas: 20,
	})
	if err == nil {
		t.Fatal("expected an error for a missing fleet, got nil")
	}
}

func TestCreateAutoscaler_RejectsZeroMaxReplicas(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "bad-max-as", Namespace: "default", Fleet: "ranked", BufferSize: "5", MaxReplicas: 0,
	})
	if err == nil {
		t.Fatal("expected an error for maxReplicas=0, got nil")
	}
}

func TestCreateAutoscaler_RejectsMinExceedingMax(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "bad-min-as", Namespace: "default", Fleet: "ranked", BufferSize: "5", MinReplicas: 30, MaxReplicas: 20,
	})
	if err == nil {
		t.Fatal("expected an error for minReplicas > maxReplicas, got nil")
	}
}

func TestCreateAutoscaler_RejectsInvalidBufferSize(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "bad-buffer-as", Namespace: "default", Fleet: "ranked", BufferSize: "not-a-value", MaxReplicas: 20,
	})
	if err == nil {
		t.Fatal("expected an error for an invalid bufferSize, got nil")
	}
}

// intstr.Parse would silently truncate this to 1 via int32 conversion; the
// hand parser must reject it instead.
func TestCreateAutoscaler_RejectsBufferSizeAboveInt32(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "huge-buffer-as", Namespace: "default", Fleet: "ranked",
		BufferSize: "4294967297", MaxReplicas: 20,
	})
	if err == nil {
		t.Fatal("expected an error for a bufferSize beyond int32 range, got nil (silent truncation to 1 would be accepted)")
	}
}

func TestCreateAutoscaler_RejectsOutOfRangePercentage(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "bad-pct-as", Namespace: "default", Fleet: "ranked", BufferSize: "150%", MaxReplicas: 20,
	})
	if err == nil {
		t.Fatal("expected an error for a bufferSize percentage over 100%, got nil")
	}
}

func int32ptr(v int32) *int32    { return &v }
func stringptr(v string) *string { return &v }

func TestUpdateAutoscaler_UpdatesBufferSize(t *testing.T) {
	existing := testAutoscaler("ranked-as", "default", "ranked", 5, 10, 20, false)
	s := newTestServer(existing)

	_, out, err := s.updateAutoscaler(context.Background(), nil, UpdateAutoscalerInput{
		Name: "ranked-as", Namespace: "default", BufferSize: stringptr("8"),
	})
	if err != nil {
		t.Fatalf("updateAutoscaler: %v", err)
	}
	if out.Autoscaler.BufferSize != "8" {
		t.Fatalf("BufferSize = %q, want 8", out.Autoscaler.BufferSize)
	}
	if out.Autoscaler.MinReplicas != 10 || out.Autoscaler.MaxReplicas != 20 {
		t.Fatalf("unrelated fields should be untouched: %+v", out.Autoscaler)
	}
}

func TestUpdateAutoscaler_UpdatesMinAndMaxIndependently(t *testing.T) {
	existing := testAutoscaler("ranked-as", "default", "ranked", 5, 10, 20, false)
	s := newTestServer(existing)

	_, out, err := s.updateAutoscaler(context.Background(), nil, UpdateAutoscalerInput{
		Name: "ranked-as", Namespace: "default", MaxReplicas: int32ptr(50),
	})
	if err != nil {
		t.Fatalf("updateAutoscaler: %v", err)
	}
	if out.Autoscaler.MaxReplicas != 50 || out.Autoscaler.MinReplicas != 10 || out.Autoscaler.BufferSize != "5" {
		t.Fatalf("unexpected result: %+v", out.Autoscaler)
	}
}

func TestUpdateAutoscaler_ExplicitZeroMinReplicasIsRespected(t *testing.T) {
	existing := testAutoscaler("ranked-as", "default", "ranked", 5, 10, 20, false)
	s := newTestServer(existing)

	_, out, err := s.updateAutoscaler(context.Background(), nil, UpdateAutoscalerInput{
		Name: "ranked-as", Namespace: "default", MinReplicas: int32ptr(0),
	})
	if err != nil {
		t.Fatalf("updateAutoscaler: %v", err)
	}
	if out.Autoscaler.MinReplicas != 0 {
		t.Fatalf("MinReplicas = %d, want 0 (an explicit 0 must not be mistaken for 'not provided')", out.Autoscaler.MinReplicas)
	}
}

func TestUpdateAutoscaler_RejectsNoFieldsProvided(t *testing.T) {
	existing := testAutoscaler("ranked-as", "default", "ranked", 5, 10, 20, false)
	s := newTestServer(existing)

	_, _, err := s.updateAutoscaler(context.Background(), nil, UpdateAutoscalerInput{Name: "ranked-as", Namespace: "default"})
	if err == nil {
		t.Fatal("expected an error when no fields are provided, got nil")
	}
}

func TestUpdateAutoscaler_RejectsResultingInvalidBounds(t *testing.T) {
	existing := testAutoscaler("ranked-as", "default", "ranked", 5, 10, 20, false)
	s := newTestServer(existing)

	_, _, err := s.updateAutoscaler(context.Background(), nil, UpdateAutoscalerInput{
		Name: "ranked-as", Namespace: "default", MinReplicas: int32ptr(30),
	})
	if err == nil {
		t.Fatal("expected an error when the update would make minReplicas exceed maxReplicas, got nil")
	}
}

func TestUpdateAutoscaler_RejectsBufferSizeAboveUnchangedMinReplicas(t *testing.T) {
	existing := testAutoscaler("ranked-as", "default", "ranked", 5, 10, 20, false)
	s := newTestServer(existing)

	_, _, err := s.updateAutoscaler(context.Background(), nil, UpdateAutoscalerInput{
		Name: "ranked-as", Namespace: "default", BufferSize: stringptr("15"),
	})
	if err == nil {
		t.Fatal("expected an error: raising bufferSize above the existing (unchanged) minReplicas of 10 should be rejected, got nil")
	}
}

func TestUpdateAutoscaler_NonexistentReturnsError(t *testing.T) {
	s := newTestServer()
	_, _, err := s.updateAutoscaler(context.Background(), nil, UpdateAutoscalerInput{
		Name: "no-such-as", Namespace: "default", BufferSize: stringptr("5"),
	})
	if err == nil {
		t.Fatal("expected an error updating a nonexistent autoscaler, got nil")
	}
}

func TestDeleteAutoscaler_DeletesExisting(t *testing.T) {
	existing := testAutoscaler("ranked-as", "default", "ranked", 5, 10, 20, false)
	s := newTestServer(existing)

	_, out, err := s.deleteAutoscaler(context.Background(), nil, DeleteAutoscalerInput{Name: "ranked-as", Namespace: "default"})
	if err != nil {
		t.Fatalf("deleteAutoscaler: %v", err)
	}
	if !out.Deleted {
		t.Fatal("Deleted = false, want true")
	}
	if _, err := testClients(s).agones.AutoscalingV1().FleetAutoscalers("default").Get(context.Background(), "ranked-as", metav1.GetOptions{}); err == nil {
		t.Error("expected autoscaler to be gone after delete")
	}
}

func TestDeleteAutoscaler_NonexistentReturnsError(t *testing.T) {
	s := newTestServer()
	_, _, err := s.deleteAutoscaler(context.Background(), nil, DeleteAutoscalerInput{Name: "no-such-as", Namespace: "default"})
	if err == nil {
		t.Fatal("expected an error deleting a nonexistent autoscaler, got nil")
	}
}
