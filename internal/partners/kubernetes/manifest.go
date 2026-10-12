package kubernetes

import (
	"sort"
	"strconv"
)

// Labels applied to every container this adapter creates. They are how the
// adapter recognises its own capacity: a namespace is shared with other
// tenants of the same cluster, and the control plane must never adopt a Pod it
// did not create.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelPartner   = "tai-talea.partner"
	LabelInstance  = "tai-talea.instance"
	// LabelNode records which machine a worker was placed on. The rotation in
	// nextNode is otherwise only visible by reading each Pod's spec, and an
	// operator checking whether two workers landed on the same node should not
	// have to.
	LabelNode = "tai-talea.node"

	managedByValue = "tai-talea"
	containerName  = "worker"
	shmVolumeName  = "shm"

	// hostnameLabel is the one node label a tenant can rely on: it is applied
	// by the kubelet itself. Custom pool labels need a Node read to verify,
	// which a tenant namespace typically cannot do.
	hostnameLabel = "kubernetes.io/hostname"

	// dnsClusterFirstWithHostNet is the only policy Kubernetes accepts for a
	// host-network Pod that still wants cluster DNS. Leaving the default
	// ClusterFirst in place makes the kubelet reject the spec and quietly fall
	// back to the node resolver.
	dnsClusterFirstWithHostNet = "ClusterFirstWithHostNet"
)

// selector is the label query that scopes a list to this partner's capacity.
func (a *Adapter) selector() string {
	return LabelManagedBy + "=" + managedByValue + "," + LabelPartner + "=" + a.partnerID
}

func (a *Adapter) labels(name string, node *NodeProfile) map[string]string {
	labels := map[string]string{
		LabelManagedBy: managedByValue,
		LabelPartner:   a.partnerID,
		LabelInstance:  name,
	}
	if node != nil {
		labels[LabelNode] = node.Host
	}
	return labels
}

// nodeSelector renders where a Pod may run. A configured per-node selector
// wins, then the node's own hostname, and with no node at all the partner's
// selector is used unchanged.
func (a *Adapter) nodeSelector(node *NodeProfile) map[string]string {
	if node == nil {
		return a.cfg.NodeSelector
	}
	if len(node.Selector) > 0 {
		return node.Selector
	}
	return map[string]string{hostnameLabel: node.Host}
}

// buildPod renders the container specification for one worker.
//
// The container carries the bootstrap itself: the entrypoint starts it with
// the shared secret and waits. The role is deliberately not baked in here —
// the planner assigns prefill or decode later, and the control plane hands it
// over through /bootstrap/start.
func (a *Adapter) buildPod(name string) Pod {
	// The machine is chosen here, before the container is written: its
	// environment is fixed at creation and cannot be revised later.
	node := a.nextNode()

	env := map[string]string{
		EnvBootstrapToken: a.cfg.BootstrapToken,
		EnvBootstrapPort:  strconv.Itoa(a.cfg.BootstrapPort),
		EnvServicePort:    strconv.Itoa(a.cfg.ServicePort),
	}
	if a.cfg.ProfilePath != "" {
		env[EnvProfile] = a.cfg.ProfilePath
	}
	for key, value := range a.cfg.Env {
		env[key] = value
	}
	// The node's environment is the most specific layer, so it is applied
	// last: a value that differs per machine must be able to win over a
	// partner-wide default.
	if node != nil {
		for key, value := range node.Env {
			env[key] = value
		}
	}

	mounts := make([]VolumeMount, 0, len(a.cfg.Volumes)+1)
	volumes := make([]Volume, 0, len(a.cfg.Volumes)+1)
	for _, volume := range a.cfg.Volumes {
		mounts = append(mounts, VolumeMount{Name: volume.Name, MountPath: volume.MountPath, ReadOnly: volume.ReadOnly})
		volumes = append(volumes, Volume{
			Name:     volume.Name,
			HostPath: &HostPathVolume{Path: volume.HostPath, Type: "Directory"},
		})
	}
	// SGLang's tensor-parallel ranks exchange activations through /dev/shm. The
	// default 64 MB is far below what an eight-way model needs, and the failure
	// is a bus error deep inside the worker rather than a clean OOM.
	if a.cfg.ShmSize != "" {
		mounts = append(mounts, VolumeMount{Name: shmVolumeName, MountPath: "/dev/shm"})
		volumes = append(volumes, Volume{Name: shmVolumeName, EmptyDir: &EmptyDirVolume{Medium: "Memory", SizeLimit: a.cfg.ShmSize}})
	}

	limits := map[string]string{}
	if a.cfg.GPUResource != "" && a.cfg.GPUCount > 0 {
		limits[a.cfg.GPUResource] = strconv.Itoa(a.cfg.GPUCount)
	}

	grace := int64(300)
	container := Container{
		Name:            containerName,
		Image:           a.cfg.Image,
		ImagePullPolicy: a.cfg.ImagePullPolicy,
		Command:         append([]string(nil), a.cfg.Entrypoint...),
		Env:             sortedEnv(env),
		Ports: []ContainerPort{
			{Name: "bootstrap", ContainerPort: a.cfg.BootstrapPort},
			{Name: "service", ContainerPort: a.cfg.ServicePort},
		},
		Resources:    ResourceRequirements{Limits: limits},
		VolumeMounts: mounts,
		SecurityContext: &SecurityContext{
			RunAsUser:  a.cfg.RunAsUser,
			RunAsGroup: a.cfg.RunAsGroup,
		},
		ReadinessProbe: &Probe{
			HTTPGet: &HTTPGetAction{
				Path: "/bootstrap/health",
				Port: a.cfg.BootstrapPort,
				HTTPHeaders: []HTTPHeader{
					{Name: "X-Bootstrap-Token", Value: a.cfg.BootstrapToken},
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			FailureThreshold:    6,
		},
	}
	if container.ImagePullPolicy == "" {
		container.ImagePullPolicy = "IfNotPresent"
	}
	// Privileged is set on the copy that goes into the Pod, before the Pod
	// literal is built: the container struct is copied by value into the
	// spec, so a later mutation of this local would never reach the API.
	if a.cfg.Privileged {
		privileged := true
		container.SecurityContext.Privileged = &privileged
	}

	pod := Pod{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata: ObjectMeta{
			Name:      name,
			Namespace: a.cfg.Namespace,
			Labels:    a.labels(name, node),
		},
		Spec: PodSpec{
			Containers:                    []Container{container},
			NodeSelector:                  a.nodeSelector(node),
			RestartPolicy:                 "Always",
			ServiceAccountName:            a.cfg.ServiceAccount,
			TerminationGracePeriodSeconds: &grace,
			Volumes:                       volumes,
		},
	}
	if a.cfg.HostNetwork {
		pod.Spec.HostNetwork = true
		// The two travel together: a host-network Pod cannot use the default
		// ClusterFirst policy, and the fallback hides every cluster name.
		pod.Spec.DNSPolicy = dnsClusterFirstWithHostNet
	}
	if a.cfg.HostIPC {
		pod.Spec.HostIPC = true
	}
	if a.cfg.RunAsUser != nil || a.cfg.RunAsGroup != nil || a.cfg.FSGroup != nil {
		pod.Spec.SecurityContext = &PodSecurityContext{
			RunAsUser:  a.cfg.RunAsUser,
			RunAsGroup: a.cfg.RunAsGroup,
			FSGroup:    a.cfg.FSGroup,
		}
	}
	return pod
}

// sortedEnv renders the environment deterministically so that two identical
// configurations never produce a different Pod spec.
func sortedEnv(values map[string]string) []EnvVar {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]EnvVar, 0, len(keys))
	for _, key := range keys {
		out = append(out, EnvVar{Name: key, Value: values[key]})
	}
	return out
}
