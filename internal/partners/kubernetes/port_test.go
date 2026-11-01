package kubernetes

import (
	"encoding/json"
	"testing"
)

// A Pod's probe port is an int-or-string. This is the shape that took the
// capacity snapshot down on the B300 cluster: the workers were deployed as
// Deployments whose probes address "bootstrap" by name, the API server echoed
// the name back, and decoding the list failed with
// "cannot unmarshal string into Go struct field ... of type int" - which turned
// every worker in the namespace invisible to the control plane.
func TestPodDecodesAProbeWithANamedPort(t *testing.T) {
	const document = `{
	  "metadata": {"name": "talea-w0", "uid": "u-1"},
	  "spec": {
	    "containers": [{
	      "name": "worker",
	      "ports": [{"name": "bootstrap", "containerPort": 9001}],
	      "readinessProbe": {"httpGet": {"path": "/bootstrap/health", "port": "bootstrap"}},
	      "livenessProbe": {"httpGet": {"path": "/bootstrap/liveness", "port": "bootstrap"}}
	    }]
	  },
	  "status": {"phase": "Running", "podIP": "10.222.2.230"}
	}`

	var pod Pod
	if err := json.Unmarshal([]byte(document), &pod); err != nil {
		t.Fatalf("decoding a named probe port failed: %v", err)
	}
	if pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port.Name != "bootstrap" {
		t.Fatalf("readiness port name = %q, want bootstrap",
			pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port.Name)
	}
	if pod.Spec.Containers[0].LivenessProbe.HTTPGet.Port.Numeric() != 0 {
		t.Fatalf("a named port must not report a number, got %d",
			pod.Spec.Containers[0].LivenessProbe.HTTPGet.Port.Numeric())
	}
}

// The adapter writes its own Pods, so the field has to marshal back to a plain
// number: a probe port sent as an object would be rejected by the API server.
func TestPortMarshalsAsItWasWritten(t *testing.T) {
	number, err := json.Marshal(Port{Number: 9001})
	if err != nil {
		t.Fatalf("marshal numeric port: %v", err)
	}
	if string(number) != "9001" {
		t.Fatalf("numeric port marshalled as %s, want 9001", number)
	}

	name, err := json.Marshal(Port{Name: "bootstrap"})
	if err != nil {
		t.Fatalf("marshal named port: %v", err)
	}
	if string(name) != `"bootstrap"` {
		t.Fatalf("named port marshalled as %s, want \"bootstrap\"", name)
	}
}
