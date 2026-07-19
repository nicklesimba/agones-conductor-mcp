package main

import (
	"context"
	"fmt"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	agonesfake "agones.dev/agones/pkg/client/clientset/versioned/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestDeleteGameServer_RefusesAllocatedWithoutForce(t *testing.T) {
	gs := testGameServer("live-match", "default", "ranked", agonesv1.GameServerStateAllocated)
	s := newTestServer(gs)

	_, out, err := s.deleteGameServer(context.Background(), nil, DeleteGameServerInput{
		Name:      "live-match",
		Namespace: "default",
	})
	if err != nil {
		t.Fatalf("deleteGameServer: %v", err)
	}
	if out.Deleted {
		t.Fatal("Deleted = true, want false: an Allocated server must never be deleted without force=true")
	}

	// Assert it's actually still there, not just that the response said so.
	still, err := testClients(s).agones.AgonesV1().GameServers("default").Get(context.Background(), "live-match", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected GameServer to still exist after refused delete, but Get failed: %v", err)
	}
	if still.Status.State != agonesv1.GameServerStateAllocated {
		t.Errorf("state changed to %s after a refused delete, want unchanged Allocated", still.Status.State)
	}
}

func TestDeleteGameServer_ForceDeletesAllocated(t *testing.T) {
	gs := testGameServer("live-match", "default", "ranked", agonesv1.GameServerStateAllocated)
	s := newTestServer(gs)

	_, out, err := s.deleteGameServer(context.Background(), nil, DeleteGameServerInput{
		Name:      "live-match",
		Namespace: "default",
		Force:     true,
	})
	if err != nil {
		t.Fatalf("deleteGameServer: %v", err)
	}
	if !out.Deleted {
		t.Fatal("Deleted = false with force=true, want true")
	}
	if out.Warning == "" {
		t.Error("expected a disconnection-consequence warning when force-deleting an Allocated server, got empty")
	}

	if _, err := testClients(s).agones.AgonesV1().GameServers("default").Get(context.Background(), "live-match", metav1.GetOptions{}); err == nil {
		t.Error("expected GameServer to be gone after force delete, but it still exists")
	}
}

func TestDeleteGameServer_ReadyDeletesWithoutForce(t *testing.T) {
	gs := testGameServer("idle-server", "default", "ranked", agonesv1.GameServerStateReady)
	s := newTestServer(gs)

	_, out, err := s.deleteGameServer(context.Background(), nil, DeleteGameServerInput{
		Name:      "idle-server",
		Namespace: "default",
	})
	if err != nil {
		t.Fatalf("deleteGameServer: %v", err)
	}
	if !out.Deleted {
		t.Error("Deleted = false, want true: a Ready (non-live) server should delete without needing force")
	}
}

// The fake doesn't enforce Delete's Preconditions, so custom reactors
// reproduce that: "get" flips the server to Allocated right after
// answering, "delete" rejects the now-stale ResourceVersion.
func TestDeleteGameServer_RaceIsClosedByResourceVersionPrecondition(t *testing.T) {
	gs := testGameServer("racy-server", "default", "ranked", agonesv1.GameServerStateReady)
	gs.ResourceVersion = "1"
	s := newTestServer(gs)

	ag, ok := testClients(s).agones.(*agonesfake.Clientset)
	if !ok {
		t.Fatal("expected the fake Agones clientset")
	}

	raced := false
	ag.PrependReactor("get", "gameservers", func(action ktesting.Action) (bool, runtime.Object, error) {
		if raced {
			return false, nil, nil
		}
		getAction := action.(ktesting.GetAction)
		obj, err := ag.Tracker().Get(gameServersGVR, getAction.GetNamespace(), getAction.GetName())
		if err != nil {
			return true, nil, err
		}
		snapshot := obj.(*agonesv1.GameServer).DeepCopy()

		raced = true
		current := obj.(*agonesv1.GameServer).DeepCopy()
		current.Status.State = agonesv1.GameServerStateAllocated
		current.ResourceVersion = "2"
		if err := ag.Tracker().Update(gameServersGVR, current, current.Namespace); err != nil {
			return true, nil, err
		}
		return true, snapshot, nil
	})

	ag.PrependReactor("delete", "gameservers", func(action ktesting.Action) (bool, runtime.Object, error) {
		delAction := action.(ktesting.DeleteAction)
		pre := delAction.GetDeleteOptions().Preconditions
		if pre == nil || pre.ResourceVersion == nil {
			return false, nil, nil
		}
		obj, err := ag.Tracker().Get(gameServersGVR, delAction.GetNamespace(), delAction.GetName())
		if err != nil {
			return true, nil, err
		}
		if obj.(*agonesv1.GameServer).ResourceVersion != *pre.ResourceVersion {
			return true, nil, apierrors.NewConflict(
				agonesv1.SchemeGroupVersion.WithResource("gameservers").GroupResource(),
				delAction.GetName(),
				fmt.Errorf("resourceVersion mismatch"),
			)
		}
		return false, nil, nil
	})

	_, out, err := s.deleteGameServer(context.Background(), nil, DeleteGameServerInput{
		Name:      "racy-server",
		Namespace: "default",
	})
	if err != nil {
		t.Fatalf("deleteGameServer: %v", err)
	}
	if out.Deleted {
		t.Fatal("Deleted = true: the server became Allocated between the safety check and the delete, and force was not set - it must be refused")
	}
	if out.State != string(agonesv1.GameServerStateAllocated) {
		t.Errorf("State = %q, want Allocated (the retry must re-check current state, not the stale Ready read)", out.State)
	}

	still, err := ag.AgonesV1().GameServers("default").Get(context.Background(), "racy-server", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected GameServer to still exist: %v", err)
	}
	if still.Status.State != agonesv1.GameServerStateAllocated {
		t.Errorf("state changed unexpectedly to %s", still.Status.State)
	}
}

func TestDeleteGameServer_NonexistentReturnsError(t *testing.T) {
	s := newTestServer()
	_, _, err := s.deleteGameServer(context.Background(), nil, DeleteGameServerInput{
		Name:      "does-not-exist",
		Namespace: "default",
	})
	if err == nil {
		t.Fatal("expected an error deleting a nonexistent GameServer, got nil")
	}
}

func TestScaleFleet_NeverTouchesAllocatedCount(t *testing.T) {
	fleet := testFleet("simple-fleet", "default", 4, 2, 2, 0, 4)
	s := newTestServer(fleet)

	_, out, err := s.scaleFleet(context.Background(), nil, ScaleFleetInput{
		Name: "simple-fleet", Namespace: "default", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("scaleFleet: %v", err)
	}
	if out.PreviousReplicas != 4 || out.TargetReplicas != 1 {
		t.Errorf("got previous=%d target=%d, want previous=4 target=1", out.PreviousReplicas, out.TargetReplicas)
	}
	if out.Allocated != 2 {
		t.Errorf("Allocated in response = %d, want 2 (unchanged; Agones fleet controller handles removal ordering, not this tool)", out.Allocated)
	}

	updated, err := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "simple-fleet", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after scale: %v", err)
	}
	if updated.Spec.Replicas != 1 {
		t.Errorf("Fleet.Spec.Replicas = %d, want 1", updated.Spec.Replicas)
	}
}

// Simulates a FleetAutoscaler writing Replicas concurrently.
func TestScaleFleet_RetriesOnConflict(t *testing.T) {
	fleet := testFleet("simple-fleet", "default", 4, 2, 2, 0, 4)
	s := newTestServer(fleet)

	ag, ok := testClients(s).agones.(*agonesfake.Clientset)
	if !ok {
		t.Fatal("expected the fake Agones clientset")
	}

	attempts := 0
	ag.PrependReactor("update", "fleets", func(action ktesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			return true, nil, apierrors.NewConflict(
				agonesv1.SchemeGroupVersion.WithResource("fleets").GroupResource(),
				"simple-fleet",
				fmt.Errorf("simulated concurrent write, e.g. by a FleetAutoscaler"),
			)
		}
		return false, nil, nil
	})

	_, out, err := s.scaleFleet(context.Background(), nil, ScaleFleetInput{
		Name: "simple-fleet", Namespace: "default", Replicas: 6,
	})
	if err != nil {
		t.Fatalf("scaleFleet: %v", err)
	}
	if out.TargetReplicas != 6 {
		t.Fatalf("TargetReplicas = %d, want 6", out.TargetReplicas)
	}
	if attempts < 2 {
		t.Fatalf("expected at least 2 update attempts after a simulated conflict, got %d", attempts)
	}
}

func TestScaleFleet_RejectsNegativeReplicas(t *testing.T) {
	s := newTestServer(testFleet("simple-fleet", "default", 4, 2, 2, 0, 4))
	_, _, err := s.scaleFleet(context.Background(), nil, ScaleFleetInput{
		Name: "simple-fleet", Namespace: "default", Replicas: -1,
	})
	if err == nil {
		t.Fatal("expected an error for negative replicas, got nil")
	}
}

func TestScaleFleet_RejectsReplicasAboveSanityCeiling(t *testing.T) {
	s := newTestServer(testFleet("simple-fleet", "default", 4, 2, 2, 0, 4))
	_, _, err := s.scaleFleet(context.Background(), nil, ScaleFleetInput{
		Name: "simple-fleet", Namespace: "default", Replicas: maxScaleFleetReplicas + 1,
	})
	if err == nil {
		t.Fatal("expected an error for a replica count above the sanity ceiling, got nil")
	}
}

func TestScaleFleet_NonexistentReturnsError(t *testing.T) {
	s := newTestServer()
	_, _, err := s.scaleFleet(context.Background(), nil, ScaleFleetInput{
		Name: "does-not-exist", Namespace: "default", Replicas: 3,
	})
	if err == nil {
		t.Fatal("expected an error scaling a nonexistent fleet, got nil")
	}
}

func TestAllocateGameServer_ReturnsAddressAndPorts(t *testing.T) {
	gs := testGameServer("simple-fleet-a", "default", "simple-fleet", agonesv1.GameServerStateReady)
	s := newTestServer(gs)

	_, out, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "simple-fleet", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("allocateGameServer: %v", err)
	}
	if out.State != "Allocated" {
		t.Errorf("State = %q, want Allocated", out.State)
	}
	if out.Address == "" {
		t.Error("expected a non-empty address on successful allocation")
	}
}

func TestAllocateGameServer_NoReadyServersReturnsUnallocatedState(t *testing.T) {
	// A fleet that exists but has no Ready servers should not error out
	// opaquely - it should come back as an unallocated result the caller
	// (and the LLM reading it) can act on.
	gs := testGameServer("simple-fleet-a", "default", "simple-fleet", agonesv1.GameServerStateAllocated)
	s := newTestServer(gs)

	_, out, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "simple-fleet", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("allocateGameServer: %v", err)
	}
	if out.State == "Allocated" {
		t.Error("expected a non-Allocated result when no Ready servers exist, got Allocated")
	}
}

func testGameServerWithCounters(name, namespace, fleet string, state agonesv1.GameServerState, counters map[string]agonesv1.CounterStatus, lists map[string]agonesv1.ListStatus) *agonesv1.GameServer {
	gs := testGameServer(name, namespace, fleet, state)
	gs.Status.Counters = counters
	gs.Status.Lists = lists
	return gs
}

func TestAllocateGameServer_CounterSelectorSkipsNonMatchingServer(t *testing.T) {
	full := testGameServerWithCounters("full", "default", "ranked", agonesv1.GameServerStateReady,
		map[string]agonesv1.CounterStatus{"players": {Count: 10, Capacity: 10}}, nil)
	room := testGameServerWithCounters("room", "default", "ranked", agonesv1.GameServerStateReady,
		map[string]agonesv1.CounterStatus{"players": {Count: 2, Capacity: 10}}, nil)
	s := newTestServer(full, room)

	_, out, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		CounterSelectors: map[string]CounterSelectorInput{"players": {MinAvailable: 1}},
	})
	if err != nil {
		t.Fatalf("allocateGameServer: %v", err)
	}
	if out.State != "Allocated" || out.GameServer != "room" {
		t.Fatalf("expected allocation of the server with room, got state=%q gameServer=%q", out.State, out.GameServer)
	}
}

func TestAllocateGameServer_CounterSelectorRejectsWhenNoneMatch(t *testing.T) {
	full := testGameServerWithCounters("full", "default", "ranked", agonesv1.GameServerStateReady,
		map[string]agonesv1.CounterStatus{"players": {Count: 10, Capacity: 10}}, nil)
	s := newTestServer(full)

	_, out, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		CounterSelectors: map[string]CounterSelectorInput{"players": {MinAvailable: 1}},
	})
	if err != nil {
		t.Fatalf("allocateGameServer: %v", err)
	}
	if out.State == "Allocated" {
		t.Error("expected no allocation when the only Ready server fails the counter selector, got Allocated")
	}
}

func TestAllocateGameServer_ListSelectorContainsValue(t *testing.T) {
	noMap := testGameServerWithCounters("no-map", "default", "ranked", agonesv1.GameServerStateReady,
		nil, map[string]agonesv1.ListStatus{"maps": {Capacity: 5, Values: []string{"dust"}}})
	hasMap := testGameServerWithCounters("has-map", "default", "ranked", agonesv1.GameServerStateReady,
		nil, map[string]agonesv1.ListStatus{"maps": {Capacity: 5, Values: []string{"dust", "sands"}}})
	s := newTestServer(noMap, hasMap)

	_, out, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		ListSelectors: map[string]ListSelectorInput{"maps": {ContainsValue: "sands"}},
	})
	if err != nil {
		t.Fatalf("allocateGameServer: %v", err)
	}
	if out.State != "Allocated" || out.GameServer != "has-map" {
		t.Fatalf("expected allocation of the server whose list contains the value, got state=%q gameServer=%q", out.State, out.GameServer)
	}
}

func TestAllocateGameServer_CounterActionIncrementsOnAllocation(t *testing.T) {
	gs := testGameServerWithCounters("gs-a", "default", "ranked", agonesv1.GameServerStateReady,
		map[string]agonesv1.CounterStatus{"players": {Count: 2, Capacity: 10}}, nil)
	s := newTestServer(gs)

	_, out, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		CounterActions: map[string]CounterActionInput{"players": {Action: "Increment", Amount: 3}},
	})
	if err != nil {
		t.Fatalf("allocateGameServer: %v", err)
	}
	if out.State != "Allocated" {
		t.Fatalf("expected Allocated, got %q", out.State)
	}
	if got := out.Counters["players"].Count; got != 5 {
		t.Fatalf("Counters[players].Count = %d, want 5", got)
	}
}

func TestAllocateGameServer_ListActionAddsValueOnAllocation(t *testing.T) {
	gs := testGameServerWithCounters("gs-a", "default", "ranked", agonesv1.GameServerStateReady,
		nil, map[string]agonesv1.ListStatus{"sessions": {Capacity: 5, Values: []string{}}})
	s := newTestServer(gs)

	_, out, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		ListActions: map[string]ListActionInput{"sessions": {AddValues: []string{"p1"}}},
	})
	if err != nil {
		t.Fatalf("allocateGameServer: %v", err)
	}
	if !containsString(out.Lists["sessions"].Values, "p1") {
		t.Fatalf("expected sessions list to contain p1, got %+v", out.Lists["sessions"])
	}
}

func TestAllocateGameServer_RejectsMissingFleet(t *testing.T) {
	s := newTestServer()
	_, _, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Namespace: "default",
	})
	if err == nil {
		t.Fatal("expected an error for a missing fleet, got nil")
	}
}

func TestAllocateGameServer_RejectsNegativeSelectorBounds(t *testing.T) {
	s := newTestServer()
	_, _, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		CounterSelectors: map[string]CounterSelectorInput{"players": {MinCount: -3}},
	})
	if err == nil {
		t.Fatal("expected an error for a negative counter selector bound, got nil")
	}
	_, _, err = s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		ListSelectors: map[string]ListSelectorInput{"maps": {MinAvailable: -1}},
	})
	if err == nil {
		t.Fatal("expected an error for a negative list selector bound, got nil")
	}
}

func TestAllocateGameServer_RejectsOversizedListActionValues(t *testing.T) {
	s := newTestServer()
	tooMany := make([]string, 1001)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("v%d", i)
	}
	_, _, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		ListActions: map[string]ListActionInput{"sessions": {AddValues: tooMany}},
	})
	if err == nil {
		t.Fatal("expected an error for more addValues than a list can ever hold, got nil")
	}
}

func TestAllocateGameServer_RejectsCounterActionWithInvalidActionName(t *testing.T) {
	s := newTestServer()
	_, _, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		CounterActions: map[string]CounterActionInput{"players": {Action: "Sideways", Amount: 1}},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid counter action name, got nil")
	}
}

func TestAllocateGameServer_RejectsCounterActionWithZeroAmount(t *testing.T) {
	s := newTestServer()
	_, _, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		CounterActions: map[string]CounterActionInput{"players": {Action: "Increment"}},
	})
	if err == nil {
		t.Fatal("expected an error for a counter action with no amount, got nil")
	}
}

func TestAllocateGameServer_RejectsCounterActionWithNegativeCapacity(t *testing.T) {
	s := newTestServer()
	neg := int64(-1)
	_, _, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		CounterActions: map[string]CounterActionInput{"players": {Capacity: &neg}},
	})
	if err == nil {
		t.Fatal("expected an error for a negative counter capacity, got nil")
	}
}

func TestAllocateGameServer_RejectsListActionCapacityOver1000(t *testing.T) {
	s := newTestServer()
	tooBig := int64(1001)
	_, _, err := s.allocateGameServer(context.Background(), nil, AllocateInput{
		Fleet: "ranked", Namespace: "default",
		ListActions: map[string]ListActionInput{"sessions": {Capacity: &tooBig}},
	})
	if err == nil {
		t.Fatal("expected an error for a list capacity over 1000, got nil")
	}
}
