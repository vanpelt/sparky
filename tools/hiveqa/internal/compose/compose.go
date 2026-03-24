// Package compose handles rewriting docker-compose YAML files for hiveqa.
// It strips host port bindings and namespaces volumes so multiple copies
// of the same compose file can run in parallel without conflicts.
package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServicePort describes a port discovered from a compose service.
type ServicePort struct {
	Service       string
	ContainerPort int
	Protocol      string
}

// RewriteResult holds the output of a Rewrite operation.
type RewriteResult struct {
	OutputPath string
	Services   []ServicePort
}

// Rewrite reads a docker-compose file, strips all ports: entries from services
// (recording what was there), namespaces all named volumes with the given prefix,
// and writes the modified file to outDir.
func Rewrite(inputPath, name, outDir string) (*RewriteResult, error) {
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, fmt.Errorf("reading compose file: %w", err)
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing compose YAML: %w", err)
	}

	prefix := "hiveqa-" + name + "-"

	// Collect discovered ports and strip them from services.
	var discovered []ServicePort
	services, ok := doc["services"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("compose file has no 'services' key")
	}

	for svcName, svcVal := range services {
		svc, ok := svcVal.(map[string]interface{})
		if !ok {
			continue
		}

		// Extract and strip ports.
		if ports, exists := svc["ports"]; exists {
			discovered = append(discovered, extractPorts(svcName, ports)...)
			delete(svc, "ports")
		}

		// Namespace volume mounts that reference named volumes.
		if vols, exists := svc["volumes"]; exists {
			svc["volumes"] = namespaceServiceVolumes(vols, prefix, doc)
		}
	}

	// Namespace top-level volume definitions.
	namespaceTopLevelVolumes(doc, prefix)

	// Write the rewritten file.
	outPath := filepath.Join(outDir, "docker-compose.yml")
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshaling rewritten compose: %w", err)
	}
	if err := os.MkdirAll(outDir, 0700); err != nil {
		return nil, fmt.Errorf("creating output dir: %w", err)
	}
	if err := os.WriteFile(outPath, out, 0644); err != nil {
		return nil, fmt.Errorf("writing rewritten compose: %w", err)
	}

	return &RewriteResult{
		OutputPath: outPath,
		Services:   discovered,
	}, nil
}

// extractPorts pulls container ports from a compose ports list.
// Handles both short ("8080:80") and long form (map with target key).
func extractPorts(svcName string, ports interface{}) []ServicePort {
	var result []ServicePort

	portList, ok := ports.([]interface{})
	if !ok {
		return nil
	}

	for _, p := range portList {
		switch v := p.(type) {
		case string:
			// Short form: "8080:80", "80", "8080:80/tcp"
			containerPort, proto := parseShortPort(v)
			if containerPort > 0 {
				result = append(result, ServicePort{
					Service:       svcName,
					ContainerPort: containerPort,
					Protocol:      proto,
				})
			}
		case map[string]interface{}:
			// Long form: {target: 80, published: 8080, protocol: tcp}
			if target, ok := v["target"]; ok {
				port := toInt(target)
				proto := "tcp"
				if p, ok := v["protocol"].(string); ok {
					proto = p
				}
				if port > 0 {
					result = append(result, ServicePort{
						Service:       svcName,
						ContainerPort: port,
						Protocol:      proto,
					})
				}
			}
		}
	}
	return result
}

// parseShortPort parses "8080:80/tcp" style port strings, returning the
// container port and protocol.
func parseShortPort(s string) (int, string) {
	proto := "tcp"
	if idx := strings.Index(s, "/"); idx >= 0 {
		proto = s[idx+1:]
		s = s[:idx]
	}

	// Could be "80", "8080:80", or "0.0.0.0:8080:80"
	parts := strings.Split(s, ":")
	last := parts[len(parts)-1]

	port := 0
	fmt.Sscanf(last, "%d", &port)
	return port, proto
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case string:
		var i int
		fmt.Sscanf(n, "%d", &i)
		return i
	}
	return 0
}

// namespaceServiceVolumes rewrites volume references in a service's volumes list.
// Named volumes (those defined in top-level volumes:) get the prefix.
// Bind mounts (paths starting with . or /) are left alone.
func namespaceServiceVolumes(vols interface{}, prefix string, doc map[string]interface{}) interface{} {
	topVols := getTopLevelVolumeNames(doc)

	volList, ok := vols.([]interface{})
	if !ok {
		return vols
	}

	result := make([]interface{}, len(volList))
	for i, v := range volList {
		switch vol := v.(type) {
		case string:
			// Short form: "volname:/container/path" or "./host:/container"
			parts := strings.SplitN(vol, ":", 2)
			if len(parts) == 2 && topVols[parts[0]] {
				result[i] = prefix + parts[0] + ":" + parts[1]
			} else {
				result[i] = vol
			}
		case map[string]interface{}:
			// Long form: {type: volume, source: volname, target: /path}
			if src, ok := vol["source"].(string); ok && topVols[src] {
				vol["source"] = prefix + src
			}
			result[i] = vol
		default:
			result[i] = v
		}
	}
	return result
}

func getTopLevelVolumeNames(doc map[string]interface{}) map[string]bool {
	names := make(map[string]bool)
	if vols, ok := doc["volumes"].(map[string]interface{}); ok {
		for name := range vols {
			names[name] = true
		}
	}
	return names
}

// namespaceTopLevelVolumes renames top-level volume definitions.
func namespaceTopLevelVolumes(doc map[string]interface{}, prefix string) {
	vols, ok := doc["volumes"].(map[string]interface{})
	if !ok {
		return
	}

	newVols := make(map[string]interface{})
	for name, cfg := range vols {
		newVols[prefix+name] = cfg
	}
	doc["volumes"] = newVols
}
