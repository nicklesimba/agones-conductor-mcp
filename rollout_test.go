package main

import (
	"context"
	"strings"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	agonesfake "agones.dev/agones/pkg/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func testTemplate(containers ...corev1.Container) agonesv1.GameServerTemplateSpec {
	return agonesv1.GameServerTemplateSpec{
		Spec: agonesv1.GameServerSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: containers},
			},
		},
	}
}

func testFleetWithTemplate(name, namespace string, replicas int32, containers ...corev1.Container) *agonesv1.Fleet {
	return &agonesv1.Fleet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agonesv1.FleetSpec{
			Replicas: replicas,
			Template: testTemplate(containers...),
		},
	}
}

func testGameServerSet(name, namespace, fleet string, containers ...corev1.Container) *agonesv1.GameServerSet {
	return &agonesv1.GameServerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{agonesv1.FleetNameLabel: fleet},
		},
		Spec: agonesv1.GameServerSetSpec{Template: testTemplate(containers...)},
	}
}

func testGameServerInSet(name, namespace, fleet, set string, state agonesv1.GameServerState) *agonesv1.GameServer {
	gs := testGameServer(name, namespace, fleet, state)
	gs.Labels[agonesv1.GameServerSetGameServerLabel] = set
	return gs
}

func TestUpdateFleetImage_PatchesContainerAndReturnsPreviousImage(t *testing.T) {
	fleet := testFleetWithTemplate("simple-fleet", "default", 3, corev1.Container{Name: "game", Image: "example/game:v1"})
	s := newTestServer(fleet)

	_, out, err := s.updateFleetImage(context.Background(), nil, UpdateFleetImageInput{
		Fleet: "simple-fleet", Namespace: "default", Image: "example/game:v2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.PreviousImage != "example/game:v1" || out.NewImage != "example/game:v2" || out.Container != "game" {
		t.Fatalf("unexpected output: %+v", out)
	}

	updated, err := testClients(s).agones.AgonesV1().Fleets("default").Get(context.Background(), "simple-fleet", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error re-fetching fleet: %v", err)
	}
	if got := updated.Spec.Template.Spec.Template.Spec.Containers[0].Image; got != "example/game:v2" {
		t.Fatalf("fleet image not persisted: got %q", got)
	}
}

func TestUpdateFleetImage_AmbiguousContainerWithoutNameErrors(t *testing.T) {
	fleet := testFleetWithTemplate("simple-fleet", "default", 3,
		corev1.Container{Name: "game", Image: "example/game:v1"},
		corev1.Container{Name: "sidecar", Image: "example/sidecar:v1"},
	)
	s := newTestServer(fleet)

	_, _, err := s.updateFleetImage(context.Background(), nil, UpdateFleetImageInput{
		Fleet: "simple-fleet", Namespace: "default", Image: "example/game:v2",
	})
	if err == nil {
		t.Fatal("expected error for ambiguous container, got nil")
	}
}

func TestUpdateFleetImage_NamedContainerSelectsCorrectOne(t *testing.T) {
	fleet := testFleetWithTemplate("simple-fleet", "default", 3,
		corev1.Container{Name: "game", Image: "example/game:v1"},
		corev1.Container{Name: "sidecar", Image: "example/sidecar:v1"},
	)
	s := newTestServer(fleet)

	_, out, err := s.updateFleetImage(context.Background(), nil, UpdateFleetImageInput{
		Fleet: "simple-fleet", Namespace: "default", Image: "example/sidecar:v2", Container: "sidecar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.PreviousImage != "example/sidecar:v1" {
		t.Fatalf("unexpected previous image: %q", out.PreviousImage)
	}
}

func TestUpdateFleetImage_RejectsEmptyImage(t *testing.T) {
	fleet := testFleetWithTemplate("simple-fleet", "default", 3, corev1.Container{Name: "game", Image: "example/game:v1"})
	s := newTestServer(fleet)

	for _, img := range []string{"", "   "} {
		_, _, err := s.updateFleetImage(context.Background(), nil, UpdateFleetImageInput{
			Fleet: "simple-fleet", Namespace: "default", Image: img,
		})
		if err == nil {
			t.Fatalf("expected an error for image %q; writing it would roll the fleet to unstartable pods", img)
		}
	}
}

func TestUpdateFleetImage_NonexistentFleetReturnsError(t *testing.T) {
	s := newTestServer()
	_, _, err := s.updateFleetImage(context.Background(), nil, UpdateFleetImageInput{
		Fleet: "no-such-fleet", Namespace: "default", Image: "example/game:v2",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent fleet, got nil")
	}
}

func TestRolloutStatus_CompleteWhenAllReplicasOnCurrentVersion(t *testing.T) {
	container := corev1.Container{Name: "game", Image: "example/game:v2"}
	fleet := testFleetWithTemplate("simple-fleet", "default", 3, container)
	gss := testGameServerSet("simple-fleet-newset", "default", "simple-fleet", container)
	s := newTestServer(fleet, gss,
		testGameServerInSet("gs-1", "default", "simple-fleet", "simple-fleet-newset", agonesv1.GameServerStateReady),
		testGameServerInSet("gs-2", "default", "simple-fleet", "simple-fleet-newset", agonesv1.GameServerStateReady),
		testGameServerInSet("gs-3", "default", "simple-fleet", "simple-fleet-newset", agonesv1.GameServerStateAllocated),
	)

	_, out, err := s.rolloutStatus(context.Background(), nil, NamedInput{Name: "simple-fleet", Namespace: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Current.GameServerSet != "simple-fleet-newset" {
		t.Fatalf("expected current set to be identified, got %+v", out.Current)
	}
	if out.Current.Replicas != 3 || out.Current.Ready != 2 || out.Current.Allocated != 1 {
		t.Fatalf("unexpected current counts: %+v", out.Current)
	}
	if len(out.Previous) != 0 {
		t.Fatalf("expected no previous versions, got %+v", out.Previous)
	}
	if !out.Complete {
		t.Fatal("expected rollout to be reported complete")
	}
	if out.PercentComplete != 100 {
		t.Fatalf("expected 100%% complete, got %v", out.PercentComplete)
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", out.Warnings)
	}
}

func TestRolloutStatus_WarnsWhenPreviousVersionHasLiveMatches(t *testing.T) {
	newContainer := corev1.Container{Name: "game", Image: "example/game:v2"}
	oldContainer := corev1.Container{Name: "game", Image: "example/game:v1"}
	fleet := testFleetWithTemplate("simple-fleet", "default", 3, newContainer)
	newSet := testGameServerSet("simple-fleet-newset", "default", "simple-fleet", newContainer)
	oldSet := testGameServerSet("simple-fleet-oldset", "default", "simple-fleet", oldContainer)
	s := newTestServer(fleet, newSet, oldSet,
		testGameServerInSet("gs-new-1", "default", "simple-fleet", "simple-fleet-newset", agonesv1.GameServerStateReady),
		testGameServerInSet("gs-new-2", "default", "simple-fleet", "simple-fleet-newset", agonesv1.GameServerStateReady),
		testGameServerInSet("gs-old-1", "default", "simple-fleet", "simple-fleet-oldset", agonesv1.GameServerStateAllocated),
	)

	_, out, err := s.rolloutStatus(context.Background(), nil, NamedInput{Name: "simple-fleet", Namespace: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Previous) != 1 || out.Previous[0].GameServerSet != "simple-fleet-oldset" || out.Previous[0].Allocated != 1 {
		t.Fatalf("unexpected previous versions: %+v", out.Previous)
	}
	if out.Complete {
		t.Fatal("expected rollout to be reported incomplete while a previous version still has replicas")
	}
	if len(out.Warnings) == 0 {
		t.Fatal("expected a warning about live matches on the previous version")
	}
	wantPercent := float64(2) / float64(3) * 100
	if out.PercentComplete != wantPercent {
		t.Fatalf("expected %v%% complete, got %v", wantPercent, out.PercentComplete)
	}
}

// Two sets matching the same template (a rollback scenario) should sum,
// not overwrite.
func TestRolloutStatus_AggregatesMultipleMatchingCurrentSets(t *testing.T) {
	container := corev1.Container{Name: "game", Image: "example/game:v2"}
	fleet := testFleetWithTemplate("simple-fleet", "default", 4, container)
	setA := testGameServerSet("simple-fleet-a", "default", "simple-fleet", container)
	setB := testGameServerSet("simple-fleet-b", "default", "simple-fleet", container)
	s := newTestServer(fleet, setA, setB,
		testGameServerInSet("gs-a-1", "default", "simple-fleet", "simple-fleet-a", agonesv1.GameServerStateReady),
		testGameServerInSet("gs-a-2", "default", "simple-fleet", "simple-fleet-a", agonesv1.GameServerStateAllocated),
		testGameServerInSet("gs-b-1", "default", "simple-fleet", "simple-fleet-b", agonesv1.GameServerStateAllocated),
	)

	_, out, err := s.rolloutStatus(context.Background(), nil, NamedInput{Name: "simple-fleet", Namespace: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Current.Replicas != 3 || out.Current.Ready != 1 || out.Current.Allocated != 2 {
		t.Fatalf("expected combined counts (replicas=3 ready=1 allocated=2), got %+v", out.Current)
	}
	if !strings.Contains(out.Current.GameServerSet, "simple-fleet-a") || !strings.Contains(out.Current.GameServerSet, "simple-fleet-b") {
		t.Fatalf("expected both set names in GameServerSet field, got %q", out.Current.GameServerSet)
	}
	if len(out.Previous) != 0 {
		t.Fatalf("expected no previous versions, got %+v", out.Previous)
	}
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "multiple GameServerSets match") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning about multiple matching GameServerSets, got %v", out.Warnings)
	}
}

func TestRolloutStatus_WarnsOnOrphanedGameServers(t *testing.T) {
	container := corev1.Container{Name: "game", Image: "example/game:v2"}
	fleet := testFleetWithTemplate("simple-fleet", "default", 2, container)
	set := testGameServerSet("simple-fleet-a", "default", "simple-fleet", container)
	orphan := testGameServer("gs-orphan", "default", "simple-fleet", agonesv1.GameServerStateAllocated)
	s := newTestServer(fleet, set,
		testGameServerInSet("gs-a-1", "default", "simple-fleet", "simple-fleet-a", agonesv1.GameServerStateReady),
		orphan,
	)

	_, out, err := s.rolloutStatus(context.Background(), nil, NamedInput{Name: "simple-fleet", Namespace: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Current.Replicas != 1 {
		t.Fatalf("expected the orphan excluded from Current's counts, got %+v", out.Current)
	}
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "no owning GameServerSet label") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning surfacing the orphaned GameServer, got %v", out.Warnings)
	}
}

// Simulates a set created between the two independent List calls.
func TestRolloutStatus_WarnsOnStrayGameServerSetNotInListing(t *testing.T) {
	container := corev1.Container{Name: "game", Image: "example/game:v2"}
	fleet := testFleetWithTemplate("simple-fleet", "default", 3, container)
	visibleSet := testGameServerSet("simple-fleet-a", "default", "simple-fleet", container)
	missingSet := testGameServerSet("simple-fleet-b", "default", "simple-fleet", container)
	s := newTestServer(fleet, visibleSet, missingSet,
		testGameServerInSet("gs-a-1", "default", "simple-fleet", "simple-fleet-a", agonesv1.GameServerStateReady),
		testGameServerInSet("gs-b-1", "default", "simple-fleet", "simple-fleet-b", agonesv1.GameServerStateAllocated),
	)

	ag, ok := testClients(s).agones.(*agonesfake.Clientset)
	if !ok {
		t.Fatal("expected the fake Agones clientset")
	}
	ag.PrependReactor("list", "gameserversets", func(action ktesting.Action) (bool, runtime.Object, error) {
		listAction := action.(ktesting.ListAction)
		obj, err := ag.Tracker().List(gameServerSetsGVR, agonesv1.SchemeGroupVersion.WithKind("GameServerSet"), listAction.GetNamespace())
		if err != nil {
			return true, nil, err
		}
		full := obj.(*agonesv1.GameServerSetList)
		filtered := &agonesv1.GameServerSetList{}
		for _, item := range full.Items {
			if item.Name != "simple-fleet-b" {
				filtered.Items = append(filtered.Items, item)
			}
		}
		return true, filtered, nil
	})

	_, out, err := s.rolloutStatus(context.Background(), nil, NamedInput{Name: "simple-fleet", Namespace: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "simple-fleet-b") && strings.Contains(w, "wasn't found") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning about the stray GameServerSet's GameServer(s), got %v", out.Warnings)
	}
}

func TestRolloutStatus_NoInitializingWarningWhenFleetIntentionallyIdle(t *testing.T) {
	fleet := testFleetWithTemplate("idle-fleet", "default", 0, corev1.Container{Name: "game", Image: "example/game:v1"})
	s := newTestServer(fleet)

	_, out, err := s.rolloutStatus(context.Background(), nil, NamedInput{Name: "idle-fleet", Namespace: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Complete || out.PercentComplete != 100 {
		t.Fatalf("expected a deliberately idle fleet to report complete, got complete=%v percent=%v", out.Complete, out.PercentComplete)
	}
	for _, w := range out.Warnings {
		if strings.Contains(w, "may still be initializing") {
			t.Fatalf("did not expect an initializing warning on an intentionally idle (0 desired) fleet, got %v", out.Warnings)
		}
	}
}

func TestRolloutStatus_NonexistentFleetReturnsError(t *testing.T) {
	s := newTestServer()
	_, _, err := s.rolloutStatus(context.Background(), nil, NamedInput{Name: "no-such-fleet", Namespace: "default"})
	if err == nil {
		t.Fatal("expected error for nonexistent fleet, got nil")
	}
}

func TestSelectContainer_SingleContainerDefaultsWithoutName(t *testing.T) {
	containers := []corev1.Container{{Name: "game", Image: "example/game:v1"}}
	idx, err := selectContainer(containers, "")
	if err != nil || idx != 0 {
		t.Fatalf("expected idx 0, nil error; got idx=%d err=%v", idx, err)
	}
}

func TestSelectContainer_MultipleContainersRequiresName(t *testing.T) {
	containers := []corev1.Container{{Name: "game"}, {Name: "sidecar"}}
	if _, err := selectContainer(containers, ""); err == nil {
		t.Fatal("expected error when container is ambiguous")
	}
}

func TestSelectContainer_UnknownNameErrors(t *testing.T) {
	containers := []corev1.Container{{Name: "game"}}
	if _, err := selectContainer(containers, "nope"); err == nil {
		t.Fatal("expected error for unknown container name")
	}
}

func TestGameServerContainerImage_UsesNamedContainerField(t *testing.T) {
	spec := agonesv1.GameServerSpec{
		Container: "game",
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "sidecar", Image: "example/sidecar:v1"},
				{Name: "game", Image: "example/game:v1"},
			}},
		},
	}
	if got := gameServerContainerImage(spec); got != "example/game:v1" {
		t.Fatalf("expected example/game:v1, got %q", got)
	}
}

func TestGameServerContainerImage_FallsBackToSoleContainer(t *testing.T) {
	spec := agonesv1.GameServerSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "game", Image: "example/game:v1"}}},
		},
	}
	if got := gameServerContainerImage(spec); got != "example/game:v1" {
		t.Fatalf("expected example/game:v1, got %q", got)
	}
}

func TestGameServerContainerImage_JoinsMultipleWhenAmbiguous(t *testing.T) {
	spec := agonesv1.GameServerSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "game", Image: "example/game:v1"},
				{Name: "sidecar", Image: "example/sidecar:v1"},
			}},
		},
	}
	want := "example/game:v1,example/sidecar:v1"
	if got := gameServerContainerImage(spec); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}
