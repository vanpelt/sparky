// Package podspec is a deliberately NARROW model of the Kubernetes objects in
// deploy/kubernetes. It covers the fields those manifests actually set and
// nothing else, and it decodes strictly, so an unrecognized field is an error
// rather than a silent drop.
//
// That is the whole point. Decoding into k8s.io/api's corev1.PodSpec would
// accept every field Kubernetes has ever had; a reader that then emits only
// the handful it understands would translate a manifest change into a local
// environment that quietly no longer matches CKS. Here, a new field is a
// compile-or-decode failure that someone has to look at.
//
// Consequences of that choice, on purpose:
//   - Adding a field to deploy/kubernetes/*.yaml means adding it here.
//   - Strict decoding also rejects duplicate keys.
//   - Quantities stay as their manifest text ("250m", "400Gi"). Nothing here
//     needs their numeric value, and parsing them would invite pretending the
//     local runtime enforces them.
package podspec

import (
	"bytes"
	"fmt"
	"io"

	yaml "gopkg.in/yaml.v2"
)

// Deployment is an apps/v1 Deployment carrying a Pod template.
type Deployment struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   Meta           `yaml:"metadata"`
	Spec       DeploymentSpec `yaml:"spec"`
}

// Meta is the subset of ObjectMeta the manifests set.
type Meta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// DeploymentSpec is the subset of apps/v1 DeploymentSpec the manifests set.
type DeploymentSpec struct {
	Replicas *int32          `yaml:"replicas"`
	Strategy *Strategy       `yaml:"strategy"`
	Selector *LabelSelector  `yaml:"selector"`
	Template PodTemplateSpec `yaml:"template"`
}

// Strategy is the rollout strategy. Both node and gateway use Recreate: the
// node owns exclusive devices and both own SQLite files.
type Strategy struct {
	Type string `yaml:"type"`
}

// LabelSelector is the equality-only selector form the manifests use.
type LabelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// PodTemplateSpec is the Pod embedded in a Deployment.
type PodTemplateSpec struct {
	Metadata Meta    `yaml:"metadata"`
	Spec     PodSpec `yaml:"spec"`
}

// PodSpec is the subset of v1.PodSpec the manifests set.
type PodSpec struct {
	ServiceAccountName            string              `yaml:"serviceAccountName"`
	AutomountServiceAccountToken  *bool               `yaml:"automountServiceAccountToken"`
	TerminationGracePeriodSeconds *int64              `yaml:"terminationGracePeriodSeconds"`
	SecurityContext               *PodSecurityContext `yaml:"securityContext"`
	NodeSelector                  map[string]string   `yaml:"nodeSelector"`
	InitContainers                []Container         `yaml:"initContainers"`
	Containers                    []Container         `yaml:"containers"`
	Volumes                       []Volume            `yaml:"volumes"`
}

// PodSecurityContext is the Pod-level security context. fsGroup is the field
// that matters here: it is how the hostPath data volume becomes writable by
// the non-root controller UID.
type PodSecurityContext struct {
	RunAsNonRoot        *bool           `yaml:"runAsNonRoot"`
	RunAsUser           *int64          `yaml:"runAsUser"`
	RunAsGroup          *int64          `yaml:"runAsGroup"`
	FSGroup             *int64          `yaml:"fsGroup"`
	FSGroupChangePolicy string          `yaml:"fsGroupChangePolicy"`
	SeccompProfile      *SeccompProfile `yaml:"seccompProfile"`
}

// Container is the subset of v1.Container the manifests set.
type Container struct {
	Name            string           `yaml:"name"`
	Image           string           `yaml:"image"`
	ImagePullPolicy string           `yaml:"imagePullPolicy"`
	Command         []string         `yaml:"command"`
	Args            []string         `yaml:"args"`
	Env             []EnvVar         `yaml:"env"`
	SecurityContext *SecurityContext `yaml:"securityContext"`
	Resources       *Resources       `yaml:"resources"`
	Ports           []ContainerPort  `yaml:"ports"`
	VolumeMounts    []VolumeMount    `yaml:"volumeMounts"`
	StartupProbe    *Probe           `yaml:"startupProbe"`
	ReadinessProbe  *Probe           `yaml:"readinessProbe"`
	LivenessProbe   *Probe           `yaml:"livenessProbe"`
}

// EnvVar is one environment entry. ValueFrom is only used by the gateway,
// whose secrets come from the cluster.
type EnvVar struct {
	Name      string        `yaml:"name"`
	Value     string        `yaml:"value"`
	ValueFrom *EnvVarSource `yaml:"valueFrom"`
}

// EnvVarSource is the indirect env form used in gateway-deployment.yaml.
type EnvVarSource struct {
	SecretKeyRef *SecretKeySelector `yaml:"secretKeyRef"`
}

// SecretKeySelector names one key inside a Secret.
type SecretKeySelector struct {
	Name     string `yaml:"name"`
	Key      string `yaml:"key"`
	Optional *bool  `yaml:"optional"`
}

// SecurityContext is the container-level security context. Every field here is
// a privilege decision that a local reproduction has to carry over verbatim or
// declare that it did not.
type SecurityContext struct {
	AllowPrivilegeEscalation *bool            `yaml:"allowPrivilegeEscalation"`
	AppArmorProfile          *AppArmorProfile `yaml:"appArmorProfile"`
	Capabilities             *Capabilities    `yaml:"capabilities"`
	Privileged               *bool            `yaml:"privileged"`
	ReadOnlyRootFilesystem   *bool            `yaml:"readOnlyRootFilesystem"`
	RunAsNonRoot             *bool            `yaml:"runAsNonRoot"`
	RunAsUser                *int64           `yaml:"runAsUser"`
	RunAsGroup               *int64           `yaml:"runAsGroup"`
	SeccompProfile           *SeccompProfile  `yaml:"seccompProfile"`
}

// Capabilities is the add/drop pair.
type Capabilities struct {
	Add  []string `yaml:"add"`
	Drop []string `yaml:"drop"`
}

// SeccompProfile selects RuntimeDefault, Unconfined, or Localhost.
type SeccompProfile struct {
	Type             string `yaml:"type"`
	LocalhostProfile string `yaml:"localhostProfile"`
}

// AppArmorProfile selects RuntimeDefault, Unconfined, or Localhost.
type AppArmorProfile struct {
	Type             string `yaml:"type"`
	LocalhostProfile string `yaml:"localhostProfile"`
}

// Resources holds the scheduler's requests and the kubelet's limits, including
// the sparkbox.dev/* extended resources served by internal/deviceplugin.
type Resources struct {
	Requests ResourceList `yaml:"requests"`
	Limits   ResourceList `yaml:"limits"`
}

// ResourceList maps a resource name to its quantity text.
type ResourceList map[string]Quantity

// Quantity is a resource quantity kept as the text the manifest wrote.
type Quantity string

// UnmarshalYAML accepts the scalar forms Kubernetes allows for a quantity:
// "250m", 400Gi, and a bare number such as 2 all arrive here.
func (q *Quantity) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar interface{}
	if err := unmarshal(&scalar); err != nil {
		return err
	}
	switch v := scalar.(type) {
	case string:
		*q = Quantity(v)
	case int:
		*q = Quantity(fmt.Sprintf("%d", v))
	case int64:
		*q = Quantity(fmt.Sprintf("%d", v))
	case float64:
		*q = Quantity(fmt.Sprintf("%v", v))
	default:
		return fmt.Errorf("resource quantity must be a scalar, got %T", scalar)
	}
	return nil
}

// ContainerPort is a declared port. Only the node's metrics port and the
// gateway's ssh/https ports exist today.
type ContainerPort struct {
	Name          string `yaml:"name"`
	ContainerPort int32  `yaml:"containerPort"`
	Protocol      string `yaml:"protocol"`
}

// VolumeMount is one mount into a container. SubPath is load-bearing on the
// node: the controller sees /var/lib/sparkbox read-only with a small set of
// writable subdirectories mounted back over it.
type VolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	SubPath   string `yaml:"subPath"`
	ReadOnly  bool   `yaml:"readOnly"`
}

// Probe is the subset of v1.Probe the manifests set.
type Probe struct {
	Exec                *ExecAction      `yaml:"exec"`
	TCPSocket           *TCPSocketAction `yaml:"tcpSocket"`
	InitialDelaySeconds int32            `yaml:"initialDelaySeconds"`
	PeriodSeconds       int32            `yaml:"periodSeconds"`
	TimeoutSeconds      int32            `yaml:"timeoutSeconds"`
	SuccessThreshold    int32            `yaml:"successThreshold"`
	FailureThreshold    int32            `yaml:"failureThreshold"`
}

// ExecAction is a probe that runs a command in the container.
type ExecAction struct {
	Command []string `yaml:"command"`
}

// TCPSocketAction is a probe that opens a TCP connection to a port, named or
// numbered.
type TCPSocketAction struct {
	Port IntOrString `yaml:"port"`
}

// IntOrString is Kubernetes' port form: a number or a declared port name.
type IntOrString struct {
	IsInt  bool
	IntVal int32
	StrVal string
}

// UnmarshalYAML accepts either scalar form.
func (v *IntOrString) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar interface{}
	if err := unmarshal(&scalar); err != nil {
		return err
	}
	switch s := scalar.(type) {
	case string:
		v.IsInt, v.StrVal = false, s
	case int:
		v.IsInt, v.IntVal = true, int32(s)
	case int64:
		v.IsInt, v.IntVal = true, int32(s)
	default:
		return fmt.Errorf("port must be a number or a port name, got %T", scalar)
	}
	return nil
}

// String renders the value the way the manifest wrote it.
func (v IntOrString) String() string {
	if v.IsInt {
		return fmt.Sprintf("%d", v.IntVal)
	}
	return v.StrVal
}

// Volume is one Pod volume. Exactly one source field is set.
type Volume struct {
	Name                  string           `yaml:"name"`
	HostPath              *HostPathVolume  `yaml:"hostPath"`
	EmptyDir              *EmptyDirVolume  `yaml:"emptyDir"`
	Secret                *SecretVolume    `yaml:"secret"`
	PersistentVolumeClaim *PVCVolume       `yaml:"persistentVolumeClaim"`
	Projected             *ProjectedVolume `yaml:"projected"`
}

// HostPathVolume is a node-local directory. The node Pod's data volume is one:
// /mnt/local is the only CKS storage that supports the reflinks a VM clone
// needs.
type HostPathVolume struct {
	Path string `yaml:"path"`
	Type string `yaml:"type"`
}

// EmptyDirVolume is a per-Pod scratch volume.
type EmptyDirVolume struct {
	Medium    string   `yaml:"medium"`
	SizeLimit Quantity `yaml:"sizeLimit"`
}

// SecretVolume mounts a cluster Secret.
type SecretVolume struct {
	SecretName  string      `yaml:"secretName"`
	DefaultMode *int32      `yaml:"defaultMode"`
	Optional    *bool       `yaml:"optional"`
	Items       []KeyToPath `yaml:"items"`
}

// KeyToPath projects one Secret key at a chosen path.
type KeyToPath struct {
	Key  string `yaml:"key"`
	Path string `yaml:"path"`
	Mode *int32 `yaml:"mode"`
}

// PVCVolume is the gateway's durable RWX claim.
type PVCVolume struct {
	ClaimName string `yaml:"claimName"`
	ReadOnly  *bool  `yaml:"readOnly"`
}

// ProjectedVolume is modeled so that introducing one is a decode-time decision
// rather than a silent drop. Nothing uses it yet.
type ProjectedVolume struct {
	DefaultMode *int32            `yaml:"defaultMode"`
	Sources     []ProjectedSource `yaml:"sources"`
}

// ProjectedSource is one member of a projected volume.
type ProjectedSource struct {
	Secret              *SecretVolume        `yaml:"secret"`
	ServiceAccountToken *ServiceAccountToken `yaml:"serviceAccountToken"`
}

// ServiceAccountToken is a projected workload-identity token.
type ServiceAccountToken struct {
	Audience          string `yaml:"audience"`
	ExpirationSeconds *int64 `yaml:"expirationSeconds"`
	Path              string `yaml:"path"`
}

// Service is a v1 Service. Only the public LoadBalancer exists in these files.
type Service struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   Meta        `yaml:"metadata"`
	Spec       ServiceSpec `yaml:"spec"`
}

// ServiceSpec is the subset of v1.ServiceSpec the manifests set.
type ServiceSpec struct {
	Type                  string            `yaml:"type"`
	ExternalTrafficPolicy string            `yaml:"externalTrafficPolicy"`
	Selector              map[string]string `yaml:"selector"`
	Ports                 []ServicePort     `yaml:"ports"`
}

// ServicePort is one published port.
type ServicePort struct {
	Name       string      `yaml:"name"`
	Protocol   string      `yaml:"protocol"`
	Port       int32       `yaml:"port"`
	TargetPort IntOrString `yaml:"targetPort"`
	NodePort   int32       `yaml:"nodePort"`
}

// NetworkPolicy is one document of network-policy.yaml: either a
// networking.k8s.io/v1 NetworkPolicy or a cilium.io/v2 CiliumNetworkPolicy.
// The two are modeled as one struct because strict decoding already tells them
// apart — a NetworkPolicy has no endpointSelector, a CiliumNetworkPolicy has no
// podSelector — and the reader that cares (internal/devpod) has to look at
// both to say which of them the local pod does not enforce.
type NetworkPolicy struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   Meta              `yaml:"metadata"`
	Spec       NetworkPolicySpec `yaml:"spec"`
}

// NetworkPolicySpec is the union of the two spellings the file uses.
type NetworkPolicySpec struct {
	// PodSelector is the k8s spelling. An empty selector (`{}`) is the
	// default-deny document and decodes to a non-nil, empty LabelSelector.
	PodSelector *LabelSelector `yaml:"podSelector"`
	// EndpointSelector is Cilium's spelling of the same thing.
	EndpointSelector *LabelSelector `yaml:"endpointSelector"`
	PolicyTypes      []string       `yaml:"policyTypes"`
	Ingress          []NetworkRule  `yaml:"ingress"`
	Egress           []NetworkRule  `yaml:"egress"`
}

// NetworkRule is one ingress or egress rule. k8s rules use ports; Cilium
// spells the same idea toPorts and adds toServices and toCIDRSet.
type NetworkRule struct {
	Ports      []NetworkPolicyPort `yaml:"ports"`
	ToPorts    []NetworkPolicyPeer `yaml:"toPorts"`
	ToServices []ServiceRef        `yaml:"toServices"`
	ToCIDRSet  []CIDRRule          `yaml:"toCIDRSet"`
}

// NetworkPolicyPeer is a Cilium toPorts entry.
type NetworkPolicyPeer struct {
	Ports []NetworkPolicyPort `yaml:"ports"`
}

// NetworkPolicyPort is one port/protocol pair. k8s writes the port as a
// number, Cilium as a string, so it is an IntOrString.
type NetworkPolicyPort struct {
	Port     IntOrString `yaml:"port"`
	Protocol string      `yaml:"protocol"`
}

// ServiceRef is a Cilium toServices entry.
type ServiceRef struct {
	K8sService *K8sServiceRef `yaml:"k8sService"`
}

// K8sServiceRef names a Service by namespace.
type K8sServiceRef struct {
	ServiceName string `yaml:"serviceName"`
	Namespace   string `yaml:"namespace"`
}

// CIDRRule is one toCIDRSet entry: a permitted prefix minus the exceptions
// that make the rule "the public internet and nothing local".
type CIDRRule struct {
	CIDR   string   `yaml:"cidr"`
	Except []string `yaml:"except"`
}

// DecodeNetworkPolicies strictly decodes every document in a multi-document
// policy file. Unlike the other manifests this one holds several objects,
// because a policy set is only meaningful as a set.
func DecodeNetworkPolicies(name string, data []byte) ([]NetworkPolicy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.SetStrict(true)
	var out []NetworkPolicy
	for {
		var policy NetworkPolicy
		err := dec.Decode(&policy)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		switch policy.Kind {
		case "NetworkPolicy", "CiliumNetworkPolicy":
		default:
			return nil, fmt.Errorf("%s: document %d is kind %q; this file holds NetworkPolicy and CiliumNetworkPolicy objects", name, len(out)+1, policy.Kind)
		}
		out = append(out, policy)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no policy documents", name)
	}
	return out, nil
}

// DecodeDeployment strictly decodes one Deployment document.
func DecodeDeployment(name string, data []byte) (*Deployment, error) {
	var out Deployment
	if err := decodeStrict(name, data, &out); err != nil {
		return nil, err
	}
	if out.Kind != "Deployment" {
		return nil, fmt.Errorf("%s: expected kind Deployment, got %q", name, out.Kind)
	}
	return &out, nil
}

// DecodeService strictly decodes one Service document.
func DecodeService(name string, data []byte) (*Service, error) {
	var out Service
	if err := decodeStrict(name, data, &out); err != nil {
		return nil, err
	}
	if out.Kind != "Service" {
		return nil, fmt.Errorf("%s: expected kind Service, got %q", name, out.Kind)
	}
	return &out, nil
}

// decodeStrict rejects unknown and duplicated fields, and rejects a second
// document: these files hold exactly one object each and deploy.sh applies
// them one at a time.
func decodeStrict(name string, data []byte, out interface{}) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.SetStrict(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	var extra interface{}
	if err := dec.Decode(&extra); err == nil {
		return fmt.Errorf("%s: expected a single document", name)
	}
	return nil
}

// FindContainer returns the named container from either list.
func (p *PodSpec) FindContainer(name string) (*Container, bool) {
	for i := range p.InitContainers {
		if p.InitContainers[i].Name == name {
			return &p.InitContainers[i], true
		}
	}
	for i := range p.Containers {
		if p.Containers[i].Name == name {
			return &p.Containers[i], true
		}
	}
	return nil, false
}

// FindVolume returns the named Pod volume.
func (p *PodSpec) FindVolume(name string) (*Volume, bool) {
	for i := range p.Volumes {
		if p.Volumes[i].Name == name {
			return &p.Volumes[i], true
		}
	}
	return nil, false
}
