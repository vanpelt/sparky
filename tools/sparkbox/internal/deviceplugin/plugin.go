// Package deviceplugin exposes the host devices needed by the Sparkbox VM
// node through Kubernetes' stable device-plugin API.
package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	ResourceKVM  = "sparkbox.dev/kvm"
	ResourceTUN  = "sparkbox.dev/tun"
	ResourceLoop = "sparkbox.dev/loop"

	defaultRetryInterval  = time.Second
	defaultHealthInterval = 2 * time.Second
)

// Device describes one host device granted for an allocation. HealthPath is
// the read-only view mounted into the plugin Pod; HostPath is resolved by the
// kubelet on the node and is never opened by the plugin.
type Device struct {
	HostPath      string
	ContainerPath string
	HealthPath    string
	Permissions   string
}

// Resource is one extended resource advertised to the kubelet. Sparkbox uses
// one synthetic ID for each shared-device bundle: there should be at most one
// long-lived Sparkbox VM-node Pod on a physical node.
type Resource struct {
	Name       string
	SocketName string
	DeviceID   string
	Devices    []Device
}

// Options controls the kubelet connection and health views. Zero intervals
// use the production defaults.
type Options struct {
	PluginDir      string
	KubeletSocket  string
	HealthRoot     string
	RetryInterval  time.Duration
	HealthInterval time.Duration
	Resources      []Resource
	Logger         *log.Logger
}

// DefaultResources returns the three device bundles needed by the current VM
// node. KVM and TUN are separate resources for clear health and scheduling
// signals. The loop bundle is temporary while guest-disk preparation still
// mounts ext4 images on the host; it avoids making the application Pod
// privileged merely to see every host device.
func DefaultResources(healthRoot string) []Resource {
	device := func(hostPath string) Device {
		relative := strings.TrimPrefix(hostPath, "/dev/")
		return Device{
			HostPath:      hostPath,
			ContainerPath: hostPath,
			HealthPath:    filepath.Join(healthRoot, relative),
			Permissions:   "rwm",
		}
	}

	loops := []Device{device("/dev/loop-control")}
	for i := 0; i < 8; i++ {
		loops = append(loops, device(fmt.Sprintf("/dev/loop%d", i)))
	}

	return []Resource{
		{
			Name:       ResourceKVM,
			SocketName: "sparkbox-kvm.sock",
			DeviceID:   "kvm0",
			Devices:    []Device{device("/dev/kvm")},
		},
		{
			Name:       ResourceTUN,
			SocketName: "sparkbox-tun.sock",
			DeviceID:   "tun0",
			Devices:    []Device{device("/dev/net/tun")},
		},
		{
			Name:       ResourceLoop,
			SocketName: "sparkbox-loop.sock",
			DeviceID:   "loop-bundle0",
			Devices:    loops,
		},
	}
}

// Run serves all configured resources until ctx is cancelled. Each resource
// reconnects independently after a kubelet restart or socket removal.
func Run(ctx context.Context, opts Options) error {
	if opts.PluginDir == "" {
		opts.PluginDir = pluginapi.DevicePluginPath
	}
	if opts.KubeletSocket == "" {
		opts.KubeletSocket = pluginapi.KubeletSocket
	}
	if opts.HealthRoot == "" {
		opts.HealthRoot = "/host-dev"
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = defaultRetryInterval
	}
	if opts.HealthInterval <= 0 {
		opts.HealthInterval = defaultHealthInterval
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if len(opts.Resources) == 0 {
		opts.Resources = DefaultResources(opts.HealthRoot)
	}
	if err := validate(opts); err != nil {
		return err
	}

	errCh := make(chan error, len(opts.Resources))
	for _, resource := range opts.Resources {
		resource := resource
		go func() {
			errCh <- runResource(ctx, opts, resource)
		}()
	}

	for range opts.Resources {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func validate(opts Options) error {
	if !filepath.IsAbs(opts.PluginDir) || !filepath.IsAbs(opts.KubeletSocket) {
		return errors.New("device-plugin paths must be absolute")
	}
	seenNames := make(map[string]bool, len(opts.Resources))
	seenSockets := make(map[string]bool, len(opts.Resources))
	for _, resource := range opts.Resources {
		if resource.Name == "" || !strings.Contains(resource.Name, "/") {
			return fmt.Errorf("invalid extended resource name %q", resource.Name)
		}
		if resource.DeviceID == "" || len(resource.Devices) == 0 {
			return fmt.Errorf("resource %s has no device ID or devices", resource.Name)
		}
		if filepath.Base(resource.SocketName) != resource.SocketName || resource.SocketName == "." {
			return fmt.Errorf("invalid socket name %q", resource.SocketName)
		}
		if seenNames[resource.Name] || seenSockets[resource.SocketName] {
			return fmt.Errorf("duplicate resource or socket for %s", resource.Name)
		}
		seenNames[resource.Name] = true
		seenSockets[resource.SocketName] = true
		for _, device := range resource.Devices {
			if !filepath.IsAbs(device.HostPath) || !filepath.IsAbs(device.ContainerPath) ||
				!filepath.IsAbs(device.HealthPath) || device.Permissions == "" {
				return fmt.Errorf("resource %s has an invalid device path or permissions", resource.Name)
			}
		}
	}
	return nil
}

func runResource(ctx context.Context, opts Options, resource Resource) error {
	for {
		err := serveOnce(ctx, opts, resource)
		if ctx.Err() != nil {
			return nil
		}
		opts.Logger.Printf("device plugin %s restarting after error: %v", resource.Name, err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.RetryInterval):
		}
	}
}

func serveOnce(ctx context.Context, opts Options, resource Resource) error {
	socketPath := filepath.Join(opts.PluginDir, resource.SocketName)
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket %s: %w", socketPath, err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	defer func() {
		listener.Close()      //nolint:errcheck
		os.Remove(socketPath) //nolint:errcheck
	}()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("protect socket %s: %w", socketPath, err)
	}

	server := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(server, &plugin{
		resource:       resource,
		healthInterval: opts.HealthInterval,
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer server.Stop()

	registrationCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = register(registrationCtx, opts.KubeletSocket, resource)
	cancel()
	if err != nil {
		return err
	}
	kubeletSocketInfo, err := os.Stat(opts.KubeletSocket)
	if err != nil {
		return fmt.Errorf("stat registered kubelet socket: %w", err)
	}
	opts.Logger.Printf("device plugin %s registered with %s", resource.Name, opts.KubeletSocket)

	ticker := time.NewTicker(opts.HealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErr:
			return fmt.Errorf("serve %s: %w", resource.Name, err)
		case <-ticker.C:
			if _, err := os.Stat(socketPath); err != nil {
				return fmt.Errorf("plugin socket disappeared: %w", err)
			}
			current, err := os.Stat(opts.KubeletSocket)
			if err != nil || !os.SameFile(kubeletSocketInfo, current) {
				return errors.New("kubelet registration socket changed")
			}
		}
	}
}

func register(ctx context.Context, kubeletSocket string, resource Resource) error {
	conn, err := grpc.DialContext( //nolint:staticcheck // DialContext supports the kubelet's Unix socket on all supported gRPC releases.
		ctx,
		"passthrough:///kubelet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", kubeletSocket)
		}),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("connect to kubelet socket: %w", err)
	}
	defer conn.Close() //nolint:errcheck
	_, err = pluginapi.NewRegistrationClient(conn).Register(ctx, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     resource.SocketName,
		ResourceName: resource.Name,
	})
	if err != nil {
		return fmt.Errorf("register %s: %w", resource.Name, err)
	}
	return nil
}

type plugin struct {
	pluginapi.UnimplementedDevicePluginServer
	resource       Resource
	healthInterval time.Duration
}

func (p *plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

func (p *plugin) ListAndWatch(_ *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	health := p.health()
	if err := stream.Send(p.listResponse(health)); err != nil {
		return err
	}
	ticker := time.NewTicker(p.healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
			current := p.health()
			if current == health {
				continue
			}
			health = current
			if err := stream.Send(p.listResponse(health)); err != nil {
				return err
			}
		}
	}
}

func (p *plugin) Allocate(_ context.Context, request *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	if p.health() != pluginapi.Healthy {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is unhealthy", p.resource.Name)
	}
	if len(request.GetContainerRequests()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "allocation has no container requests")
	}
	response := &pluginapi.AllocateResponse{}
	for _, containerRequest := range request.GetContainerRequests() {
		ids := containerRequest.GetDevicesIds()
		if len(ids) != 1 || ids[0] != p.resource.DeviceID {
			return nil, status.Errorf(codes.InvalidArgument, "invalid %s device IDs %q", p.resource.Name, ids)
		}
		containerResponse := &pluginapi.ContainerAllocateResponse{}
		for _, device := range p.resource.Devices {
			containerResponse.Devices = append(containerResponse.Devices, &pluginapi.DeviceSpec{
				HostPath:      device.HostPath,
				ContainerPath: device.ContainerPath,
				Permissions:   device.Permissions,
			})
		}
		response.ContainerResponses = append(response.ContainerResponses, containerResponse)
	}
	return response, nil
}

func (p *plugin) health() string {
	for _, device := range p.resource.Devices {
		info, err := os.Stat(device.HealthPath)
		if err != nil || info.Mode()&os.ModeDevice == 0 {
			return pluginapi.Unhealthy
		}
	}
	return pluginapi.Healthy
}

func (p *plugin) listResponse(health string) *pluginapi.ListAndWatchResponse {
	return &pluginapi.ListAndWatchResponse{Devices: []*pluginapi.Device{{
		ID:     p.resource.DeviceID,
		Health: health,
	}}}
}
