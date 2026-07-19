package main

import (
	"context"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCreateFleet_CreatesWithDefaults(t *testing.T) {
	s := newTestServer()

	_, out, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "new-fleet", Namespace: "default", Replicas: 3,
		Image: "example/game:v1", ContainerPort: 7654,
	})
	if err != nil {
		t.Fatalf("createFleet: %v", err)
	}
	if out.Fleet.Name != "new-fleet" || out.Fleet.Desired != 3 {
		t.Fatalf("unexpected output: %+v", out.Fleet)
	}

	created, err := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "new-fleet", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	container := created.Spec.Template.Spec.Template.Spec.Containers[0]
	if container.Image != "example/game:v1" || container.Name != defaultGameServerContainerName {
		t.Fatalf("unexpected container: %+v", container)
	}
	if created.Spec.Template.Spec.Ports[0].ContainerPort != 7654 {
		t.Fatalf("unexpected port: %+v", created.Spec.Template.Spec.Ports[0])
	}
	if created.Spec.Template.Spec.Ports[0].PortPolicy != agonesv1.Dynamic {
		t.Fatalf("expected default PortPolicy Dynamic, got %v", created.Spec.Template.Spec.Ports[0].PortPolicy)
	}
}

func TestCreateFleet_AppliesResourcesAndScheduling(t *testing.T) {
	s := newTestServer()

	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "res-fleet", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 7000,
		CPURequest: "100m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "256Mi",
		Scheduling: "Distributed",
	})
	if err != nil {
		t.Fatalf("createFleet: %v", err)
	}

	created, err := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "res-fleet", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	res := created.Spec.Template.Spec.Template.Spec.Containers[0].Resources
	if res.Requests.Cpu().String() != "100m" || res.Limits.Cpu().String() != "500m" {
		t.Fatalf("unexpected CPU resources: %+v", res)
	}
	if res.Requests.Memory().String() != "128Mi" || res.Limits.Memory().String() != "256Mi" {
		t.Fatalf("unexpected memory resources: %+v", res)
	}
}

func TestCreateFleet_DeclaresInitialCountersAndLists(t *testing.T) {
	s := newTestServer()

	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "cl-fleet", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 7000,
		Counters: map[string]CounterInitInput{"players": {Count: 2, Capacity: 10}},
		Lists:    map[string]ListInitInput{"sessions": {Capacity: 5, Values: []string{"s1"}}},
	})
	if err != nil {
		t.Fatalf("createFleet: %v", err)
	}

	created, err := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "cl-fleet", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	counters := created.Spec.Template.Spec.Counters
	if counters["players"].Count != 2 || counters["players"].Capacity != 10 {
		t.Fatalf("unexpected initial counter: %+v", counters["players"])
	}
	lists := created.Spec.Template.Spec.Lists
	if lists["sessions"].Capacity != 5 || len(lists["sessions"].Values) != 1 || lists["sessions"].Values[0] != "s1" {
		t.Fatalf("unexpected initial list: %+v", lists["sessions"])
	}
}

func TestCreateFleet_RejectsCounterCountAboveCapacity(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "bad-counter-fleet", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 7000,
		Counters: map[string]CounterInitInput{"players": {Count: 20, Capacity: 10}},
	})
	if err == nil {
		t.Fatal("expected an error for an initial counter count above capacity, got nil")
	}
}

func TestCreateFleet_RejectsListCapacityOver1000(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "bad-list-fleet", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 7000,
		Lists: map[string]ListInitInput{"sessions": {Capacity: 1001}},
	})
	if err == nil {
		t.Fatal("expected an error for a list capacity over 1000, got nil")
	}
}

func TestCreateFleet_RejectsMoreInitialValuesThanListCapacity(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "overfull-list-fleet", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 7000,
		Lists: map[string]ListInitInput{"sessions": {Capacity: 1, Values: []string{"a", "b"}}},
	})
	if err == nil {
		t.Fatal("expected an error when initial values exceed list capacity, got nil")
	}
}

func TestCreateFleet_RejectsInvalidResourceQuantity(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "bad-fleet", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 7000, CPURequest: "not-a-quantity",
	})
	if err == nil {
		t.Fatal("expected an error for an invalid CPU quantity, got nil")
	}
}

func TestCreateFleet_RejectsMissingImage(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "no-image", Namespace: "default", Replicas: 1, ContainerPort: 7000,
	})
	if err == nil {
		t.Fatal("expected an error for a missing image, got nil")
	}
}

func TestCreateFleet_RejectsInvalidContainerPort(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "bad-port", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 0,
	})
	if err == nil {
		t.Fatal("expected an error for containerPort=0, got nil")
	}
}

func TestCreateFleet_RejectsInvalidPortPolicy(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "bad-policy", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 7000, PortPolicy: "Sideways",
	})
	if err == nil {
		t.Fatal("expected an error for an invalid portPolicy, got nil")
	}
}

func TestDeleteFleet_RefusesWithAllocatedGameServers(t *testing.T) {
	fleet := testFleet("live-fleet", "default", 2, 1, 1, 0, 2)
	allocated := testGameServer("live-fleet-a", "default", "live-fleet", agonesv1.GameServerStateAllocated)
	s := newTestServer(fleet, allocated)

	_, out, err := s.deleteFleet(context.Background(), nil, DeleteFleetInput{Name: "live-fleet", Namespace: "default"})
	if err != nil {
		t.Fatalf("deleteFleet: %v", err)
	}
	if out.Deleted {
		t.Fatal("Deleted = true, want false: a fleet with a live match must not be deleted without force=true")
	}
	if out.Allocated != 1 {
		t.Fatalf("Allocated = %d, want 1", out.Allocated)
	}

	if _, err := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "live-fleet", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected fleet to still exist after refused delete: %v", err)
	}
}

func TestDeleteFleet_ForceDeletesWithAllocatedGameServers(t *testing.T) {
	fleet := testFleet("live-fleet", "default", 2, 1, 1, 0, 2)
	allocated := testGameServer("live-fleet-a", "default", "live-fleet", agonesv1.GameServerStateAllocated)
	s := newTestServer(fleet, allocated)

	_, out, err := s.deleteFleet(context.Background(), nil, DeleteFleetInput{Name: "live-fleet", Namespace: "default", Force: true})
	if err != nil {
		t.Fatalf("deleteFleet: %v", err)
	}
	if !out.Deleted {
		t.Fatal("Deleted = false with force=true, want true")
	}
	if out.Warning == "" {
		t.Error("expected a disconnection-consequence warning when force-deleting a fleet with live matches")
	}

	if _, err := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "live-fleet", metav1.GetOptions{}); err == nil {
		t.Error("expected fleet to be gone after force delete")
	}
}

func TestDeleteFleet_NoAllocatedDeletesWithoutForce(t *testing.T) {
	fleet := testFleet("idle-fleet", "default", 1, 1, 0, 0, 1)
	ready := testGameServer("idle-fleet-a", "default", "idle-fleet", agonesv1.GameServerStateReady)
	s := newTestServer(fleet, ready)

	_, out, err := s.deleteFleet(context.Background(), nil, DeleteFleetInput{Name: "idle-fleet", Namespace: "default"})
	if err != nil {
		t.Fatalf("deleteFleet: %v", err)
	}
	if !out.Deleted {
		t.Error("Deleted = false, want true: a fleet with no live matches should delete without needing force")
	}
}

func TestDeleteFleet_NonexistentReturnsError(t *testing.T) {
	s := newTestServer()
	_, _, err := s.deleteFleet(context.Background(), nil, DeleteFleetInput{Name: "no-such-fleet", Namespace: "default"})
	if err == nil {
		t.Fatal("expected an error deleting a nonexistent fleet, got nil")
	}
}
