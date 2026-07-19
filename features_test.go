package main

import (
	"context"
	"strings"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCreateFleet_TCPProtocol(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "ws-fleet", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 3000, Protocol: "TCP",
	})
	if err != nil {
		t.Fatalf("createFleet: %v", err)
	}
	created, err := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "ws-fleet", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if got := created.Spec.Template.Spec.Ports[0].Protocol; got != corev1.ProtocolTCP {
		t.Fatalf("Protocol = %q, want TCP", got)
	}
}

func TestCreateFleet_DefaultsToUDPAndRejectsInvalidProtocol(t *testing.T) {
	s := newTestServer()
	_, _, err := s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "udp-fleet", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 7000,
	})
	if err != nil {
		t.Fatalf("createFleet: %v", err)
	}
	created, _ := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "udp-fleet", metav1.GetOptions{})
	if got := created.Spec.Template.Spec.Ports[0].Protocol; got != corev1.ProtocolUDP {
		t.Fatalf("default Protocol = %q, want UDP", got)
	}

	_, _, err = s.createFleet(context.Background(), nil, CreateFleetInput{
		Name: "bad-proto", Namespace: "default", Replicas: 1,
		Image: "example/game:v1", ContainerPort: 7000, Protocol: "SCTP",
	})
	if err == nil {
		t.Fatal("expected an error for an unsupported protocol, got nil")
	}
}

func TestBuildAllocation_PreferReuseOrdersAllocatedFirst(t *testing.T) {
	alloc, err := buildAllocation(AllocateInput{Fleet: "ranked", PreferReuse: true})
	if err != nil {
		t.Fatalf("buildAllocation: %v", err)
	}
	sels := alloc.Spec.Selectors
	if len(sels) != 2 {
		t.Fatalf("expected 2 ordered selectors with preferReuse, got %d", len(sels))
	}
	if sels[0].GameServerState == nil || *sels[0].GameServerState != agonesv1.GameServerStateAllocated {
		t.Fatalf("first selector must prefer Allocated servers, got %+v", sels[0].GameServerState)
	}
	if sels[1].GameServerState == nil || *sels[1].GameServerState != agonesv1.GameServerStateReady {
		t.Fatalf("fallback selector must target Ready servers, got %+v", sels[1].GameServerState)
	}
}

func TestBuildAllocation_MetadataPatchAndSingleSelectorDefault(t *testing.T) {
	alloc, err := buildAllocation(AllocateInput{
		Fleet:       "ranked",
		Labels:      map[string]string{"match-id": "m-123"},
		Annotations: map[string]string{"mode": "ranked-2v2"},
	})
	if err != nil {
		t.Fatalf("buildAllocation: %v", err)
	}
	if len(alloc.Spec.Selectors) != 1 {
		t.Fatalf("expected a single selector without preferReuse, got %d", len(alloc.Spec.Selectors))
	}
	if alloc.Spec.MetaPatch.Labels["match-id"] != "m-123" || alloc.Spec.MetaPatch.Annotations["mode"] != "ranked-2v2" {
		t.Fatalf("metadata patch not carried: %+v", alloc.Spec.MetaPatch)
	}

	if _, err := buildAllocation(AllocateInput{Fleet: "ranked", Labels: map[string]string{"": "x"}}); err == nil {
		t.Fatal("expected an error for an empty label key, got nil")
	}
}

func TestCreateAutoscaler_CounterPolicy(t *testing.T) {
	s := newTestServer()
	_, out, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "slots-as", Namespace: "default", Fleet: "ranked", Policy: "Counter",
		Counter: &CapacityPolicyInput{Key: "players", BufferSize: "20", MaxCapacity: 1000},
	})
	if err != nil {
		t.Fatalf("createAutoscaler: %v", err)
	}
	if out.Autoscaler.PolicyType != string(autoscalingv1.CounterPolicyType) || out.Autoscaler.Key != "players" || out.Autoscaler.MaxCapacity != 1000 {
		t.Fatalf("unexpected summary: %+v", out.Autoscaler)
	}
	created, err := testClients(s).agones.AutoscalingV1().FleetAutoscalers("default").Get(context.Background(), "slots-as", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if created.Spec.Policy.Counter == nil || created.Spec.Policy.Counter.Key != "players" {
		t.Fatalf("Counter policy not persisted: %+v", created.Spec.Policy)
	}
}

func TestCreateAutoscaler_CounterPolicyValidation(t *testing.T) {
	s := newTestServer()
	cases := []CreateAutoscalerInput{
		{Name: "a", Namespace: "default", Fleet: "f", Policy: "Counter"},                                                                             // missing counter
		{Name: "a", Namespace: "default", Fleet: "f", Policy: "Counter", Counter: &CapacityPolicyInput{BufferSize: "5", MaxCapacity: 10}},            // missing key
		{Name: "a", Namespace: "default", Fleet: "f", Policy: "Counter", Counter: &CapacityPolicyInput{Key: "p", BufferSize: "5"}},                   // maxCapacity 0
		{Name: "a", Namespace: "default", Fleet: "f", Policy: "Counter", Counter: &CapacityPolicyInput{Key: "p", BufferSize: "50", MaxCapacity: 10}}, // buffer > max
		{Name: "a", Namespace: "default", Fleet: "f", Policy: "Sideways"},                                                                            // bad policy
		{Name: "a", Namespace: "default", Fleet: "f", BufferSize: "5", MaxReplicas: 10, Counter: &CapacityPolicyInput{Key: "p"}},                     // counter without policy
	}
	for i, in := range cases {
		if _, _, err := s.createAutoscaler(context.Background(), nil, in); err == nil {
			t.Errorf("case %d: expected a validation error, got nil", i)
		}
	}
}

func TestCreateAutoscaler_SyncInterval(t *testing.T) {
	s := newTestServer()
	_, out, err := s.createAutoscaler(context.Background(), nil, CreateAutoscalerInput{
		Name: "fast-as", Namespace: "default", Fleet: "ranked",
		BufferSize: "5", MaxReplicas: 20, SyncIntervalSeconds: 5,
	})
	if err != nil {
		t.Fatalf("createAutoscaler: %v", err)
	}
	if out.Autoscaler.SyncIntervalSeconds != 5 {
		t.Fatalf("SyncIntervalSeconds = %d, want 5", out.Autoscaler.SyncIntervalSeconds)
	}
	created, _ := testClients(s).agones.AutoscalingV1().FleetAutoscalers("default").Get(context.Background(), "fast-as", metav1.GetOptions{})
	if created.Spec.Sync == nil || created.Spec.Sync.FixedInterval.Seconds != 5 {
		t.Fatalf("Sync not persisted: %+v", created.Spec.Sync)
	}
}

func TestUpdateAutoscaler_SyncIntervalOnly(t *testing.T) {
	existing := testAutoscaler("ranked-as", "default", "ranked", 5, 10, 20, false)
	s := newTestServer(existing)

	five := int32(5)
	_, out, err := s.updateAutoscaler(context.Background(), nil, UpdateAutoscalerInput{
		Name: "ranked-as", Namespace: "default", SyncIntervalSeconds: &five,
	})
	if err != nil {
		t.Fatalf("updateAutoscaler: %v", err)
	}
	if out.Autoscaler.SyncIntervalSeconds != 5 || out.Autoscaler.BufferSize != "5" {
		t.Fatalf("expected sync updated and buffer untouched: %+v", out.Autoscaler)
	}
}

func TestUpdateFleetEnv_SetOverrideAndUnset(t *testing.T) {
	container := corev1.Container{Name: "game", Image: "example/game:v1", Env: []corev1.EnvVar{
		{Name: "KEEP", Value: "old"},
		{Name: "OVERRIDE", Value: "old"},
		{Name: "REMOVE", Value: "old"},
		{Name: "SECRET", ValueFrom: &corev1.EnvVarSource{}},
	}}
	fleet := testFleetWithTemplate("env-fleet", "default", 2, container)
	s := newTestServer(fleet)

	_, out, err := s.updateFleetEnv(context.Background(), nil, UpdateFleetEnvInput{
		Fleet: "env-fleet", Namespace: "default",
		Set:   map[string]string{"OVERRIDE": "new", "ADDED": "fresh"},
		Unset: []string{"REMOVE"},
	})
	if err != nil {
		t.Fatalf("updateFleetEnv: %v", err)
	}
	joined := strings.Join(out.Env, ";")
	for _, want := range []string{"KEEP=old", "OVERRIDE=new", "ADDED=fresh", "SECRET=(from source)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in resulting env, got %v", want, out.Env)
		}
	}
	if strings.Contains(joined, "REMOVE") {
		t.Errorf("REMOVE should be gone, got %v", out.Env)
	}

	updated, _ := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "env-fleet", metav1.GetOptions{})
	if len(updated.Spec.Template.Spec.Template.Spec.Containers[0].Env) != 4 {
		t.Fatalf("persisted env has %d entries, want 4", len(updated.Spec.Template.Spec.Template.Spec.Containers[0].Env))
	}
}

func TestUpdateFleetEnv_RejectsConflictsAndEmptyInput(t *testing.T) {
	fleet := testFleetWithTemplate("env-fleet", "default", 1, corev1.Container{Name: "game"})
	s := newTestServer(fleet)

	if _, _, err := s.updateFleetEnv(context.Background(), nil, UpdateFleetEnvInput{Fleet: "env-fleet", Namespace: "default"}); err == nil {
		t.Fatal("expected an error when neither set nor unset is provided")
	}
	_, _, err := s.updateFleetEnv(context.Background(), nil, UpdateFleetEnvInput{
		Fleet: "env-fleet", Namespace: "default",
		Set: map[string]string{"X": "1"}, Unset: []string{"X"},
	})
	if err == nil {
		t.Fatal("expected an error when a name appears in both set and unset")
	}
}

func TestGetGameServer_ReturnsFullDetail(t *testing.T) {
	gs := testGameServerWithCounters("gs-detail", "default", "ranked", agonesv1.GameServerStateAllocated,
		map[string]agonesv1.CounterStatus{"players": {Count: 3, Capacity: 10}}, nil)
	gs.Labels["match-id"] = "m-42"
	gs.Labels[agonesv1.GameServerSetGameServerLabel] = "ranked-abc"
	gs.Annotations = map[string]string{"note": "vip"}
	s := newTestServer(gs)

	_, out, err := s.getGameServer(context.Background(), nil, GetGameServerInput{Name: "gs-detail", Namespace: "default"})
	if err != nil {
		t.Fatalf("getGameServer: %v", err)
	}
	if out.State != "Allocated" || out.GameServerSet != "ranked-abc" || out.Labels["match-id"] != "m-42" || out.Annotations["note"] != "vip" {
		t.Fatalf("unexpected detail: %+v", out)
	}
	if out.Counters["players"].Count != 3 {
		t.Fatalf("counters missing: %+v", out.Counters)
	}
}

func TestGetGameServer_NonexistentReturnsError(t *testing.T) {
	s := newTestServer()
	if _, _, err := s.getGameServer(context.Background(), nil, GetGameServerInput{Name: "nope", Namespace: "default"}); err == nil {
		t.Fatal("expected an error for a nonexistent GameServer")
	}
}

func TestFleetEvents_IncludesFleetAndGameServerSetEvents(t *testing.T) {
	container := corev1.Container{Name: "game", Image: "example/game:v1"}
	fleet := testFleetWithTemplate("ranked", "default", 2, container)
	gss := testGameServerSet("ranked-abc", "default", "ranked", container)
	fleetEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-f", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Fleet", Name: "ranked"},
		Type:           "Normal", Reason: "ScalingFleet", Message: "Scaling fleet from 2 to 5",
	}
	gssEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-g", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "GameServerSet", Name: "ranked-abc"},
		Type:           "Normal", Reason: "SuccessfulCreate", Message: "created GameServer",
	}
	unrelated := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-x", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Service", Name: "ranked"},
		Type:           "Normal", Reason: "Provisioned",
	}
	s := newTestServer(fleet, gss, fleetEvent, gssEvent, unrelated)

	_, out, err := s.fleetEvents(context.Background(), nil, FleetEventsInput{Name: "ranked", Namespace: "default"})
	if err != nil {
		t.Fatalf("fleetEvents: %v", err)
	}
	if len(out.Events) != 2 {
		t.Fatalf("expected Fleet + GameServerSet events, got %d: %+v", len(out.Events), out.Events)
	}
}

func TestAutoscalerEvents_ReturnsOnlyAutoscalerEvents(t *testing.T) {
	evt := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-as", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "FleetAutoscaler", Name: "ranked-as"},
		Type:           "Normal", Reason: "AutoScalingFleet", Message: "Scaling fleet ranked from 2 to 4",
	}
	other := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-o", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Fleet", Name: "ranked-as"},
		Type:           "Normal", Reason: "Whatever",
	}
	s := newTestServer(evt, other)

	_, out, err := s.autoscalerEvents(context.Background(), nil, AutoscalerEventsInput{Name: "ranked-as", Namespace: "default"})
	if err != nil {
		t.Fatalf("autoscalerEvents: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].Reason != "AutoScalingFleet" {
		t.Fatalf("expected exactly the autoscaler event, got %+v", out.Events)
	}
}

func testAgonesPod(name, role string, ready bool, restarts int32) *corev1.Pod {
	phase := corev1.PodRunning
	cond := corev1.ConditionTrue
	if !ready {
		cond = corev1.ConditionFalse
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "agones-system",
			Labels:    map[string]string{"app": "agones", "agones.dev/role": role},
		},
		Status: corev1.PodStatus{
			Phase:             phase,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
			ContainerStatuses: []corev1.ContainerStatus{{RestartCount: restarts}},
		},
	}
}

func TestAgonesHealth_HealthyCluster(t *testing.T) {
	s := newTestServer(
		testAgonesPod("controller-1", "controller", true, 0),
		testAgonesPod("allocator-1", "allocator", true, 2),
	)
	_, out, err := s.agonesHealth(context.Background(), nil, AgonesHealthInput{})
	if err != nil {
		t.Fatalf("agonesHealth: %v", err)
	}
	if !out.Healthy || len(out.Components) != 2 {
		t.Fatalf("expected healthy with 2 components, got %+v", out)
	}
}

func TestAgonesHealth_ControllerDownIsUnhealthy(t *testing.T) {
	s := newTestServer(testAgonesPod("controller-1", "controller", false, 12))
	_, out, err := s.agonesHealth(context.Background(), nil, AgonesHealthInput{})
	if err != nil {
		t.Fatalf("agonesHealth: %v", err)
	}
	if out.Healthy {
		t.Fatal("expected unhealthy when the only controller pod is not ready")
	}
	if len(out.Warnings) == 0 {
		t.Fatal("expected a warning naming the down component")
	}
}

func TestAgonesHealth_NoAgonesInstalledWarnsClearly(t *testing.T) {
	s := newTestServer()
	_, out, err := s.agonesHealth(context.Background(), nil, AgonesHealthInput{})
	if err != nil {
		t.Fatalf("agonesHealth: %v", err)
	}
	if out.Healthy || len(out.Warnings) == 0 {
		t.Fatalf("expected an unhealthy result with an is-Agones-installed warning, got %+v", out)
	}
}

func TestDryRun_FlagIsEchoedInOutputs(t *testing.T) {
	fleet := testFleet("dry-fleet", "default", 2, 2, 0, 0, 2)
	s := newTestServer(fleet)

	_, out, err := s.scaleFleet(context.Background(), nil, ScaleFleetInput{
		Name: "dry-fleet", Namespace: "default", Replicas: 5, DryRun: true,
	})
	if err != nil {
		t.Fatalf("scaleFleet dryRun: %v", err)
	}
	if !out.DryRun {
		t.Fatal("expected DryRun=true echoed in the output")
	}
}
