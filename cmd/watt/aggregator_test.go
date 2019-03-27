package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/datawire/teleproxy/pkg/consulwatch"

	"github.com/datawire/teleproxy/pkg/watt"

	"github.com/datawire/teleproxy/pkg/k8s"
	"github.com/datawire/teleproxy/pkg/supervisor"
)

type aggIsolator struct {
	snapshots  chan string
	watches    chan []k8s.Resource
	aggregator *aggregator
	sup        *supervisor.Supervisor
	done       chan struct{}
	t          *testing.T
	cancel     context.CancelFunc
}

func newAggIsolator(t *testing.T, requiredKinds []string) *aggIsolator {
	// aggregator uses zero length channels for its inputs so we can
	// control the total ordering of all inputs and therefore
	// intentionally trigger any order of events we want to test
	iso := &aggIsolator{
		// we need to create buffered channels for outputs
		// because nothing is asynchronously reading them in
		// the test
		watches:   make(chan []k8s.Resource, 100),
		snapshots: make(chan string, 100),
		// for signaling when the isolator is done
		done: make(chan struct{}),
	}
	iso.aggregator = NewAggregator(iso.snapshots, iso.watches, requiredKinds)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	iso.cancel = cancel
	iso.sup = supervisor.WithContext(ctx)
	iso.sup.Supervise(&supervisor.Worker{
		Name: "aggregator",
		Work: iso.aggregator.Work,
	})
	iso.t = t
	return iso
}

func startAggIsolator(t *testing.T, requiredKinds []string) *aggIsolator {
	iso := newAggIsolator(t, requiredKinds)
	iso.Start()
	return iso
}

func (iso *aggIsolator) Start() {
	go func() {
		errs := iso.sup.Run()
		if len(errs) > 0 {
			iso.t.Errorf("unexpected errors: %v", errs)
		}
		close(iso.done)
	}()
}

func (iso *aggIsolator) Stop() {
	iso.sup.Shutdown()
	iso.cancel()
	<-iso.done
}

func resources(input string) []k8s.Resource {
	result, err := k8s.ParseResources("aggregator-test", input)
	if err != nil {
		panic(err)
	}
	return result
}

var (
	SERVICES = resources(`
---
kind: Service
apiVersion: v1
metadata:
  name: foo
spec:
  selector:
    pod: foo
  ports:
  - protocol: TCP
    port: 80
    targetPort: 80
`)
	RESOLVER = resources(`
---
kind: ConfigMap
apiVersion: v1
metadata:
  name: bar
  annotations:
    "getambassador.io/consul-resolver": "true"
data:
  consulAddress: "127.0.0.1:8500"
  datacenter: "dc1"
  service: "bar"
`)
)

// make sure we shutdown even before achieving a bootstrapped state
func TestAggregatorShutdown(t *testing.T) {
	iso := startAggIsolator(t, nil)
	defer iso.Stop()
}

// Check that we bootstrap properly... this means *not* emitting a
// snapshot until we have:
//
//   a) achieved synchronization with the kubernetes API server
//
//   b) received (possibly empty) endpoint info about all referenced
//      consul services...
func TestAggregatorBootstrap(t *testing.T) {
	iso := startAggIsolator(t, []string{"service", "configmap"})
	defer iso.Stop()

	// initial kubernetes state is just services
	iso.aggregator.KubernetesEvents <- k8sEvent{"service", SERVICES}
	// whenever the aggregator sees updated k8s state, it should
	// send an update to the consul watch manager, in this case it
	// will be empty because there are no resolvers yet
	expect(t, iso.watches, []k8s.Resource(nil))

	// we should not generate a snapshot yet because we specified
	// configmaps are required
	expect(t, iso.snapshots, Timeout(100*time.Millisecond))

	// the configmap references a consul service, so we shouldn't
	// get a snapshot yet, but we should get watches
	iso.aggregator.KubernetesEvents <- k8sEvent{"configmap", RESOLVER}
	expect(t, iso.snapshots, Timeout(100*time.Millisecond))
	expect(t, iso.watches, func(watches []k8s.Resource) bool {
		if len(watches) != 1 {
			return false
		}

		if watches[0].Name() != "bar" {
			return false
		}

		return true
	})

	// now lets send in the first endpoints, and we should get a
	// snapshot
	iso.aggregator.ConsulEndpoints <- consulwatch.Endpoints{
		Service: "bar",
		Endpoints: []consulwatch.Endpoint{
			{
				Service: "bar",
				Address: "1.2.3.4",
				Port:    80,
			},
		},
	}

	expect(t, iso.snapshots, func(snapshot string) bool {
		s := &watt.Snapshot{}
		err := json.Unmarshal([]byte(snapshot), s)
		if err != nil {
			return false
		}
		_, ok := s.Consul.Endpoints["bar"]
		return ok
	})
}
