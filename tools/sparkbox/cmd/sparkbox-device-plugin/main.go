package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/deviceplugin"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func main() {
	pluginDir := flag.String("plugin-dir", pluginapi.DevicePluginPath, "kubelet device-plugin socket directory")
	kubeletSocket := flag.String("kubelet-socket", pluginapi.KubeletSocket, "kubelet device-plugin registration socket")
	healthRoot := flag.String("health-root", "/host-dev", "read-only root containing host device health views")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := deviceplugin.Run(ctx, deviceplugin.Options{
		PluginDir:     *pluginDir,
		KubeletSocket: *kubeletSocket,
		HealthRoot:    *healthRoot,
		Logger:        logger,
	}); err != nil {
		logger.Fatalf("device plugin: %v", err)
	}
}
