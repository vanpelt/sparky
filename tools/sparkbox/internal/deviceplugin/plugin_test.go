package deviceplugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestDefaultResources(t *testing.T) {
	resources := DefaultResources("/host-dev")
	if len(resources) != 3 {
		t.Fatalf("resource count = %d, want 3", len(resources))
	}
	want := []string{ResourceKVM, ResourceTUN, ResourceLoop}
	for i, name := range want {
		if resources[i].Name != name {
			t.Errorf("resource %d name = %q, want %q", i, resources[i].Name, name)
		}
	}
	if got := resources[0].Devices[0].HealthPath; got != "/host-dev/kvm" {
		t.Errorf("KVM health path = %q", got)
	}
	if got := resources[1].Devices[0].HealthPath; got != "/host-dev/net/tun" {
		t.Errorf("TUN health path = %q", got)
	}
	if got := len(resources[2].Devices); got != 9 {
		t.Errorf("loop bundle device count = %d, want 9", got)
	}
}

func TestValidateRejectsDuplicateResource(t *testing.T) {
	resource := testResource("/dev/null")
	err := validate(Options{
		PluginDir:     "/plugins",
		KubeletSocket: "/plugins/kubelet.sock",
		Resources:     []Resource{resource, resource},
	})
	if err == nil {
		t.Fatal("validate accepted duplicate resource")
	}
}

func TestHealthRequiresDeviceNode(t *testing.T) {
	regular := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(regular, []byte("not a device"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &plugin{resource: testResource(regular), healthInterval: time.Millisecond}
	if got := p.health(); got != pluginapi.Unhealthy {
		t.Fatalf("regular-file health = %q, want %q", got, pluginapi.Unhealthy)
	}
	p.resource.Devices[0].HealthPath = "/dev/null"
	if got := p.health(); got != pluginapi.Healthy {
		t.Fatalf("character-device health = %q, want %q", got, pluginapi.Healthy)
	}
}

func TestAllocateReturnsOnlyConfiguredDevice(t *testing.T) {
	p := &plugin{resource: testResource("/dev/null"), healthInterval: time.Millisecond}
	response, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{
			DevicesIds: []string{p.resource.DeviceID},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ContainerResponses) != 1 || len(response.ContainerResponses[0].Devices) != 1 {
		t.Fatalf("allocation response = %#v", response)
	}
	device := response.ContainerResponses[0].Devices[0]
	if device.HostPath != "/dev/example" || device.ContainerPath != "/dev/example" || device.Permissions != "rwm" {
		t.Fatalf("allocated device = %#v", device)
	}
}

func TestAllocateRejectsUnknownID(t *testing.T) {
	p := &plugin{resource: testResource("/dev/null"), healthInterval: time.Millisecond}
	_, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{{DevicesIds: []string{"other"}}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error = %v, code = %v, want InvalidArgument", err, status.Code(err))
	}
}

func testResource(healthPath string) Resource {
	return Resource{
		Name:       "example.test/device",
		SocketName: "example.sock",
		DeviceID:   "device0",
		Devices: []Device{{
			HostPath:      "/dev/example",
			ContainerPath: "/dev/example",
			HealthPath:    healthPath,
			Permissions:   "rwm",
		}},
	}
}
