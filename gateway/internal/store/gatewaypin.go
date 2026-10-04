package store

import (
	"path/filepath"
	"sync"

	"piso/internal/mcpchannel"
)

// gatewayPin is the last pin PublishGatewayPin stored. WriteAllWorkerEnvs
// republishes it so a slug created after boot still receives gateway.json.
var (
	pinMu      sync.RWMutex
	gatewayPin *mcpchannel.Pin
)

// PublishGatewayPin remembers pin and writes it into every workers/<slug>/.
func PublishGatewayPin(statePath string, pin mcpchannel.Pin) error {
	pinMu.Lock()
	cp := pin
	gatewayPin = &cp
	pinMu.Unlock()
	return WriteAllWorkerPins(statePath, pin)
}

func currentGatewayPin() *mcpchannel.Pin {
	pinMu.RLock()
	defer pinMu.RUnlock()
	if gatewayPin == nil {
		return nil
	}
	cp := *gatewayPin
	return &cp
}

// WorkerGatewayPinPath is $PISO_DATA/workers/<slug>/gateway.json.
func WorkerGatewayPinPath(statePath, slug string) string {
	return filepath.Join(WorkersDir(statePath), slug, mcpchannel.PinFile)
}

// WriteWorkerGatewayPin writes one slug's gateway.json.
func WriteWorkerGatewayPin(statePath, slug string, pin mcpchannel.Pin) error {
	return mcpchannel.WritePin(WorkerGatewayPinPath(statePath, slug), pin)
}

// WriteAllWorkerPins writes gateway.json for every workers/<slug>/ directory,
// including monitor. Missing workers/ is not an error.
func WriteAllWorkerPins(statePath string, pin mcpchannel.Pin) error {
	slugs, err := ListWorkerSlugs(statePath)
	if err != nil {
		return err
	}
	for _, slug := range slugs {
		if err := WriteWorkerGatewayPin(statePath, slug, pin); err != nil {
			return err
		}
	}
	return nil
}
