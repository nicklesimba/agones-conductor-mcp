package main

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestListFleets_ReportsAllReplicaCounts(t *testing.T) {
	fleet := testFleet("ranked", "default", 5, 3, 2, 0, 5)
	s := newTestServer(fleet)

	_, out, err := s.listFleets(context.Background(), nil, PagedNamespaceInput{})
	if err != nil {
		t.Fatalf("listFleets: %v", err)
	}
	if len(out.Fleets) != 1 {
		t.Fatalf("expected 1 fleet, got %d", len(out.Fleets))
	}
	got := out.Fleets[0]
	if got.Desired != 5 || got.Ready != 3 || got.Allocated != 2 || got.Total != 5 {
		t.Errorf("got %+v, want desired=5 ready=3 allocated=2 total=5", got)
	}
}

func TestListFleets_NamespaceFilter(t *testing.T) {
	a := testFleet("fleet-a", "prod", 1, 1, 0, 0, 1)
	b := testFleet("fleet-b", "staging", 1, 1, 0, 0, 1)
	s := newTestServer(a, b)

	_, out, err := s.listFleets(context.Background(), nil, PagedNamespaceInput{Namespace: "prod"})
	if err != nil {
		t.Fatalf("listFleets: %v", err)
	}
	if len(out.Fleets) != 1 || out.Fleets[0].Name != "fleet-a" {
		t.Errorf("namespace filter failed: got %+v", out.Fleets)
	}
}

func TestListAutoscalers_ReportsPolicyAndLimits(t *testing.T) {
	scaler := testAutoscaler("ranked-as", "default", "ranked", 3, 2, 10, false)
	s := newTestServer(scaler)

	_, out, err := s.listAutoscalers(context.Background(), nil, PagedNamespaceInput{})
	if err != nil {
		t.Fatalf("listAutoscalers: %v", err)
	}
	if len(out.Autoscalers) != 1 {
		t.Fatalf("expected 1 autoscaler, got %d", len(out.Autoscalers))
	}
	got := out.Autoscalers[0]
	if got.Fleet != "ranked" || got.MinReplicas != 2 || got.MaxReplicas != 10 || got.ScalingLimited {
		t.Errorf("got %+v, want fleet=ranked min=2 max=10 scalingLimited=false", got)
	}
}

func TestGameServerEvents_ReturnsMatchingEventsOnly(t *testing.T) {
	relevant := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "GameServer", Name: "target-gs"},
		Type:           "Warning",
		Reason:         "Unhealthy",
		Message:        "readiness probe failed",
		Count:          3,
	}
	podEvent := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-pod", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "target-gs"},
		Type:           "Warning",
		Reason:         "BackOff",
	}
	unrelated := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-2", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "GameServer", Name: "other-gs"},
		Type:           "Normal",
		Reason:         "Scheduled",
	}
	sameNameOtherKind := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-3", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Service", Name: "target-gs"},
		Type:           "Normal",
		Reason:         "Provisioned",
	}
	s := newTestServer(relevant, podEvent, unrelated, sameNameOtherKind)

	_, out, err := s.gameServerEvents(context.Background(), nil, GameServerEventsInput{Name: "target-gs", Namespace: "default"})
	if err != nil {
		t.Fatalf("gameServerEvents: %v", err)
	}
	if len(out.Events) != 2 {
		t.Fatalf("expected the GameServer and backing-Pod events only, got %d: %+v", len(out.Events), out.Events)
	}
	for _, e := range out.Events {
		if e.Kind != "GameServer" && e.Kind != "Pod" {
			t.Errorf("event for kind %q leaked through; a same-named Service must be excluded", e.Kind)
		}
	}
	if out.Notice == "" {
		t.Error("expected the untrusted-content notice on the events output")
	}
}

func TestGameServerEvents_NoEventsReturnsEmptyNotNil(t *testing.T) {
	s := newTestServer()
	_, out, err := s.gameServerEvents(context.Background(), nil, GameServerEventsInput{Name: "nothing-here", Namespace: "default"})
	if err != nil {
		t.Fatalf("gameServerEvents: %v", err)
	}
	if out.Events == nil {
		t.Error("Events is nil, want empty slice so JSON serializes as [] not null")
	}
}

func TestGameServerEvents_TruncatesOversizedMessage(t *testing.T) {
	huge := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "evt-huge", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "GameServer", Name: "target-gs"},
		Type:           "Warning",
		Reason:         "Unhealthy",
		Message:        strings.Repeat("x", maxEventMessageBytes+500),
	}
	s := newTestServer(huge)

	_, out, err := s.gameServerEvents(context.Background(), nil, GameServerEventsInput{Name: "target-gs", Namespace: "default"})
	if err != nil {
		t.Fatalf("gameServerEvents: %v", err)
	}
	if len(out.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(out.Events))
	}
	if len(out.Events[0].Message) > maxEventMessageBytes+100 {
		t.Fatalf("expected the message truncated near %d bytes, got %d", maxEventMessageBytes, len(out.Events[0].Message))
	}
	if !strings.Contains(out.Events[0].Message, "truncated") {
		t.Fatalf("expected a truncation marker in the message, got: %q", out.Events[0].Message)
	}
}

// The fake Pod.GetLogs() returns a fixed placeholder body, not real
// container output, so this only proves request wiring, not log content.
func TestGameServerLogs_DoesNotErrorAndReturnsSomeContent(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "some-gs", Namespace: "default"}}
	s := newTestServer(pod, testGameServer("some-gs", "default", "fleet-x", agonesv1.GameServerStateReady))

	_, out, err := s.gameServerLogs(context.Background(), nil, GameServerLogsInput{
		Name: "some-gs", Namespace: "default", TailLines: 50,
	})
	if err != nil {
		t.Fatalf("gameServerLogs: %v", err)
	}
	if out.Name != "some-gs" {
		t.Errorf("Name = %q, want %q", out.Name, "some-gs")
	}
}

func TestGameServerLogs_DefaultsTailLinesTo200(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "some-gs", Namespace: "default"}}
	s := newTestServer(pod, testGameServer("some-gs", "default", "fleet-x", agonesv1.GameServerStateReady))

	_, _, err := s.gameServerLogs(context.Background(), nil, GameServerLogsInput{
		Name: "some-gs", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("gameServerLogs with omitted tailLines: %v", err)
	}
}

func TestGameServerLogs_ContentIsDelimitedAsUntrusted(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "some-gs", Namespace: "default"}}
	s := newTestServer(pod, testGameServer("some-gs", "default", "fleet-x", agonesv1.GameServerStateReady))

	_, out, err := s.gameServerLogs(context.Background(), nil, GameServerLogsInput{
		Name: "some-gs", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("gameServerLogs: %v", err)
	}
	if !strings.HasPrefix(out.Logs, untrustedContentNotice) {
		t.Fatalf("expected Logs to start with the untrusted-content notice, got: %q", out.Logs)
	}
}

func TestGameServerLogs_RefusesPodThatIsNotAGameServer(t *testing.T) {
	// A pod exists with this name but no GameServer does: the tool must
	// refuse rather than act as a general-purpose cluster log reader.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sneaky-pod", Namespace: "default"}}
	s := newTestServer(pod)

	_, _, err := s.gameServerLogs(context.Background(), nil, GameServerLogsInput{
		Name: "sneaky-pod", Namespace: "default",
	})
	if err == nil {
		t.Fatal("expected an error reading logs of a pod that is not a GameServer, got nil")
	}
	if !strings.Contains(err.Error(), "GameServer") {
		t.Fatalf("expected the error to explain the GameServer requirement, got: %v", err)
	}
}

func TestGameServerLogs_RejectsTailLinesAboveCap(t *testing.T) {
	s := newTestServer(testGameServer("some-gs", "default", "fleet-x", agonesv1.GameServerStateReady))
	_, _, err := s.gameServerLogs(context.Background(), nil, GameServerLogsInput{
		Name: "some-gs", Namespace: "default", TailLines: maxTailLines + 1,
	})
	if err == nil {
		t.Fatal("expected an error for tailLines above the cap, got nil")
	}
}

func TestEventTimestamp_FallsBackWhenLastTimestampZero(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	e := &corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: created}}
	got := eventTimestamp(e)
	if strings.HasPrefix(got, "0001-") {
		t.Fatalf("expected a fallback timestamp, got year-1 zero value: %q", got)
	}
}

func TestTruncateEventMessage_DoesNotSplitRunes(t *testing.T) {
	msg := strings.Repeat("a", maxEventMessageBytes-1) + strings.Repeat("é", 300)
	got := truncateEventMessage(msg)
	if !utf8.ValidString(got) {
		t.Fatal("truncated event message contains an invalid UTF-8 sequence")
	}
	if !strings.Contains(got, "truncated") {
		t.Fatal("expected a truncation marker")
	}
}

func TestTruncateLogTail_KeepsShortContentIntact(t *testing.T) {
	if got := truncateLogTail("short log line"); got != "short log line" {
		t.Fatalf("expected content under the cap to pass through unchanged, got %q", got)
	}
}

func TestTruncateLogTail_TruncatesToCapWithoutSplittingRunes(t *testing.T) {
	// Padding length chosen so the byte cut lands inside the multi-byte
	// rune sequence at the front of the kept tail.
	content := strings.Repeat("a", maxLogBytes-1) + strings.Repeat("é", 200)
	got := truncateLogTail(content)
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected a truncation marker, got prefix %q", got[:40])
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated output contains an invalid UTF-8 sequence - the cut split a multi-byte rune")
	}
}

func TestGameServerLogs_RejectsNegativeTailLines(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "some-gs", Namespace: "default"}}
	s := newTestServer(pod, testGameServer("some-gs", "default", "fleet-x", agonesv1.GameServerStateReady))

	_, _, err := s.gameServerLogs(context.Background(), nil, GameServerLogsInput{
		Name: "some-gs", Namespace: "default", TailLines: -5,
	})
	if err == nil {
		t.Fatal("expected an error for negative tailLines, got nil")
	}
}
