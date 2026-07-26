package appcontainer

import (
	"encoding/json"
	"fmt"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
)

// machineDoc is `container machine inspect <id>`'s element shape, as captured
// in testdata/machine-inspect.json. Only the fields we act on are named; the
// rest (createdDate, diskSize, platform, userSetup, the image descriptor) are
// deliberately ignored so a CLI that adds keys does not break the parse.
//
// Note what is NOT here and cannot be: virtualization and the kernel path. See
// machine.ContainerInfo.
type machineDoc struct {
	ContainerID string `json:"containerId"`
	CPUs        int    `json:"cpus"`
	HomeMount   string `json:"homeMount"`
	ID          string `json:"id"`
	Image       struct {
		Reference string `json:"reference"`
	} `json:"image"`
	IPAddress string `json:"ipAddress"`
	Memory    uint64 `json:"memory"`
	Status    string `json:"status"`
}

func parseMachineInspect(b []byte, name string) (machine.Info, error) {
	var docs []machineDoc
	if err := json.Unmarshal(b, &docs); err != nil {
		return machine.Info{}, fmt.Errorf("parse `container machine inspect %s` output: %w", name, err)
	}
	if len(docs) == 0 {
		// An empty array is the same fact as a notFound error, and both happen:
		// treat them identically so callers have one case to handle.
		return machine.Info{}, machine.ErrNotFound
	}
	d := docs[0]
	return machine.Info{
		Name:        d.ID,
		ContainerID: d.ContainerID,
		ImageRef:    d.Image.Reference,
		HomeMount:   d.HomeMount,
		IPAddress:   d.IPAddress,
		State:       toState(d.Status),
		CPUs:        d.CPUs,
		MemoryBytes: d.Memory,
	}, nil
}

// toState maps the CLI's status strings onto machine.State. "running" and
// "stopped" are the two observed; anything else is reported as unknown rather
// than guessed at, because the caller's decision (adopt / start / refuse)
// differs for each.
func toState(s string) machine.State {
	switch s {
	case "running":
		return machine.StateRunning
	case "stopped":
		return machine.StateStopped
	case "":
		return machine.StateUnknown
	default:
		return machine.State(s)
	}
}

// containerDoc is `container inspect <containerId>`'s element shape, captured
// in testdata/container-inspect.json. The one field this exists for is
// configuration.virtualization.
type containerDoc struct {
	ID            string `json:"id"`
	Configuration struct {
		Virtualization bool `json:"virtualization"`
	} `json:"configuration"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func parseContainerInspect(b []byte, cid string) (machine.ContainerInfo, error) {
	var docs []containerDoc
	if err := json.Unmarshal(b, &docs); err != nil {
		return machine.ContainerInfo{}, fmt.Errorf("parse `container inspect %s` output: %w", cid, err)
	}
	if len(docs) == 0 {
		return machine.ContainerInfo{}, machine.ErrNotFound
	}
	return machine.ContainerInfo{
		Virtualization: docs[0].Configuration.Virtualization,
		State:          docs[0].Status.State,
	}, nil
}

// versionDoc is one element of `container system version --format json`.
type versionDoc struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

// parseCLIVersion picks the CLI's own version out of the array.
//
// Selecting on appName is required, not stylistic: the same array formats the
// container-apiserver element's "version" as PROSE ("container-apiserver
// version 1.1.0 (build: release, commit: 5973b9c)") while the "container"
// element carries a clean "1.1.0". Taking element [0] or the last one would
// work today and break on any reordering.
func parseCLIVersion(b []byte) (string, error) {
	var docs []versionDoc
	if err := json.Unmarshal(b, &docs); err != nil {
		return "", fmt.Errorf("parse `container system version --format json` output: %w", err)
	}
	for _, d := range docs {
		if d.AppName == "container" {
			return d.Version, nil
		}
	}
	return "", fmt.Errorf("`container system version` named no \"container\" component (got %d entries)", len(docs))
}

// statusDoc is `container system status --format json`.
type statusDoc struct {
	Status string `json:"status"`
}

func parseSystemStatus(b []byte) bool {
	var d statusDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return false
	}
	return d.Status == "running"
}
