package main

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	agonesfake "agones.dev/agones/pkg/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

var gameServersGVR = schema.GroupVersionResource{
	Group:    agonesv1.SchemeGroupVersion.Group,
	Version:  agonesv1.SchemeGroupVersion.Version,
	Resource: "gameservers",
}

var gameServerSetsGVR = schema.GroupVersionResource{
	Group:    agonesv1.SchemeGroupVersion.Group,
	Version:  agonesv1.SchemeGroupVersion.Version,
	Resource: "gameserversets",
}

// The fake clientset doesn't simulate Agones's allocator (a real aggregated
// API, not a plain CRD), so this reactor fakes the one behavior tools
// depend on: allocate the first matching Ready GameServer. It uses
// ag.Tracker() instead of ag.AgonesV1() to avoid re-entering the fake's own
// reactor lock, which deadlocks.
func newTestServer(objs ...runtime.Object) *server {
	var agonesObjs []runtime.Object
	var coreObjs []runtime.Object
	for _, o := range objs {
		switch o.(type) {
		case *agonesv1.Fleet, *agonesv1.GameServer, *agonesv1.GameServerSet, *autoscalingv1.FleetAutoscaler:
			agonesObjs = append(agonesObjs, o)
		case *corev1.Event, *corev1.Pod:
			coreObjs = append(coreObjs, o)
		}
	}
	ag := agonesfake.NewSimpleClientset(agonesObjs...)
	core := k8sfake.NewSimpleClientset(coreObjs...)

	ag.PrependReactor("create", "gameserverallocations", func(action ktesting.Action) (bool, runtime.Object, error) {
		createAction := action.(ktesting.CreateAction)
		alloc, ok := createAction.GetObject().(*allocationv1.GameServerAllocation)
		if !ok {
			return false, nil, nil
		}

		selector := labels.Everything()
		if len(alloc.Spec.Selectors) > 0 {
			sel, err := metav1.LabelSelectorAsSelector(&alloc.Spec.Selectors[0].LabelSelector)
			if err != nil {
				return true, nil, err
			}
			selector = sel
		}

		namespace := action.GetNamespace()
		listObj, err := ag.Tracker().List(gameServersGVR, agonesv1.SchemeGroupVersion.WithKind("GameServer"), namespace)
		if err != nil {
			return true, nil, err
		}
		gsList, ok := listObj.(*agonesv1.GameServerList)
		if !ok {
			return true, nil, nil
		}

		for i := range gsList.Items {
			gs := gsList.Items[i]
			if gs.Status.State != agonesv1.GameServerStateReady || !selector.Matches(labels.Set(gs.Labels)) {
				continue
			}
			if len(alloc.Spec.Selectors) > 0 && !countsAndListsMatch(gs, alloc.Spec.Selectors[0]) {
				continue
			}
			gs.Status.State = agonesv1.GameServerStateAllocated
			applyCounterAndListActions(&gs, alloc.Spec.Counters, alloc.Spec.Lists)
			if err := ag.Tracker().Update(gameServersGVR, &gs, gs.Namespace); err != nil {
				return true, nil, err
			}
			alloc.Status = allocationv1.GameServerAllocationStatus{
				State:          allocationv1.GameServerAllocationAllocated,
				GameServerName: gs.Name,
				Address:        gs.Status.Address,
				Ports:          gs.Status.Ports,
				Counters:       gs.Status.Counters,
				Lists:          gs.Status.Lists,
			}
			return true, alloc, nil
		}

		alloc.Status = allocationv1.GameServerAllocationStatus{State: allocationv1.GameServerAllocationUnAllocated}
		return true, alloc, nil
	})

	return &server{c: &registry{def: "test", byName: map[string]*clients{"test": {agones: ag, core: core}}}}
}

// countsAndListsMatch mirrors the subset of Agones's real allocator matching
// logic (CounterSelector/ListSelector) needed to test allocate_gameserver's
// selector plumbing against the fake clientset, which doesn't run the real
// aggregated allocation API.
func countsAndListsMatch(gs agonesv1.GameServer, sel allocationv1.GameServerSelector) bool {
	for name, cs := range sel.Counters {
		counter, ok := gs.Status.Counters[name]
		if !ok {
			return false
		}
		available := counter.Capacity - counter.Count
		if counter.Count < cs.MinCount {
			return false
		}
		if cs.MaxCount != 0 && counter.Count > cs.MaxCount {
			return false
		}
		if available < cs.MinAvailable {
			return false
		}
		if cs.MaxAvailable != 0 && available > cs.MaxAvailable {
			return false
		}
	}
	for name, ls := range sel.Lists {
		list, ok := gs.Status.Lists[name]
		if !ok {
			return false
		}
		available := list.Capacity - int64(len(list.Values))
		if ls.ContainsValue != "" && !containsString(list.Values, ls.ContainsValue) {
			return false
		}
		if available < ls.MinAvailable {
			return false
		}
		if ls.MaxAvailable != 0 && available > ls.MaxAvailable {
			return false
		}
	}
	return true
}

func containsString(values []string, v string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}
	return false
}

// applyCounterAndListActions mirrors the subset of Agones's real allocator
// action-application logic (CounterAction/ListAction) needed to test
// allocate_gameserver's action plumbing.
func applyCounterAndListActions(gs *agonesv1.GameServer, counters map[string]allocationv1.CounterAction, lists map[string]allocationv1.ListAction) {
	for name, ca := range counters {
		counter, ok := gs.Status.Counters[name]
		if !ok {
			continue
		}
		if ca.Action != nil && ca.Amount != nil {
			switch *ca.Action {
			case "Increment":
				counter.Count += *ca.Amount
			case "Decrement":
				counter.Count -= *ca.Amount
			}
		}
		if ca.Capacity != nil {
			counter.Capacity = *ca.Capacity
		}
		if counter.Count > counter.Capacity {
			counter.Count = counter.Capacity
		}
		if counter.Count < 0 {
			counter.Count = 0
		}
		gs.Status.Counters[name] = counter
	}
	for name, la := range lists {
		list, ok := gs.Status.Lists[name]
		if !ok {
			continue
		}
		if la.Capacity != nil {
			list.Capacity = *la.Capacity
		}
		for _, del := range la.DeleteValues {
			for i, v := range list.Values {
				if v == del {
					list.Values = append(list.Values[:i], list.Values[i+1:]...)
					break
				}
			}
		}
		for _, add := range la.AddValues {
			if containsString(list.Values, add) {
				continue
			}
			if int64(len(list.Values)) >= list.Capacity {
				continue
			}
			list.Values = append(list.Values, add)
		}
		gs.Status.Lists[name] = list
	}
}

func testClients(s *server) *clients {
	return s.c.byName[s.c.def]
}

func testFleet(name, namespace string, replicas, ready, allocated, reserved, total int32) *agonesv1.Fleet {
	return &agonesv1.Fleet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agonesv1.FleetSpec{Replicas: replicas},
		Status: agonesv1.FleetStatus{
			Replicas:          total,
			ReadyReplicas:     ready,
			AllocatedReplicas: allocated,
			ReservedReplicas:  reserved,
		},
	}
}

func testGameServer(name, namespace, fleet string, state agonesv1.GameServerState) *agonesv1.GameServer {
	gs := &agonesv1.GameServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{},
		},
		Status: agonesv1.GameServerStatus{
			State:   state,
			Address: "10.0.0.1",
			Ports:   []agonesv1.GameServerStatusPort{{Name: "default", Port: 7000}},
		},
	}
	if fleet != "" {
		gs.Labels[agonesv1.FleetNameLabel] = fleet
	}
	return gs
}

// limited=true models a ceiling clamp: ScalingLimited with desired pinned at
// max. Floor clamps (ScalingLimited at minReplicas) are built by hand in the
// test that covers them.
func testAutoscaler(name, namespace, fleet string, buffer, min, max int32, limited bool) *autoscalingv1.FleetAutoscaler {
	desired := min
	if limited {
		desired = max
	}
	return &autoscalingv1.FleetAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: autoscalingv1.FleetAutoscalerSpec{
			FleetName: fleet,
			Policy: autoscalingv1.FleetAutoscalerPolicy{
				Type: autoscalingv1.BufferPolicyType,
				Buffer: &autoscalingv1.BufferPolicy{
					BufferSize:  intstr.FromInt(int(buffer)),
					MinReplicas: min,
					MaxReplicas: max,
				},
			},
		},
		Status: autoscalingv1.FleetAutoscalerStatus{
			ScalingLimited:  limited,
			DesiredReplicas: desired,
		},
	}
}
