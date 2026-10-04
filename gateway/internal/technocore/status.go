package technocore

import (
	"os"
	"sync"
	"time"

	tc "piso/internal/technocore"
)

// Status is a snapshot for the local dashboard.
type Status struct {
	Enrolled     bool      `json:"enrolled"`
	Status       string    `json:"status"`
	Name         string    `json:"name"`
	ServerURL    string    `json:"serverUrl"`
	Fingerprint  string    `json:"fingerprint"`
	PrincipalID  string    `json:"principalId,omitempty"`
	Placement    string    `json:"placement,omitempty"`
	PairURL      string    `json:"pairUrl,omitempty"`
	LastSync     time.Time `json:"lastSync,omitempty"`
	LastError    string    `json:"lastError,omitempty"`
	EnvelopeSkip int       `json:"envelopeSkip"`
}

var (
	statusMu   sync.Mutex
	liveStatus Status
)

func setLive(mut func(*Status)) {
	statusMu.Lock()
	defer statusMu.Unlock()
	mut(&liveStatus)
}

// Snapshot returns enrollment plus last sync information.
func Snapshot(dataDir string) Status {
	statusMu.Lock()
	out := liveStatus
	statusMu.Unlock()
	id, err := tc.Load(tc.Path(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			out.Status = "unpaired"
			return out
		}
		out.LastError = err.Error()
		out.Status = "error"
		return out
	}
	out.Enrolled = id.Status == tc.StatusActive
	out.Status = displayStatus(id.Status)
	out.Name = id.Name
	out.ServerURL = id.ServerURL
	out.Fingerprint = id.Fingerprint()
	out.PrincipalID = id.PrincipalID
	out.Placement = id.PlacementOrLocal()
	if id.Status == tc.StatusPending && id.PairingID != "" {
		out.PairURL = id.ServerURL + "/pair/" + id.PairingID + "/"
	}
	return out
}

// displayStatus maps a freshly generated identity to the same chip label as
// a missing file. Enrollment still requires active.
func displayStatus(status string) string {
	switch status {
	case "", tc.StatusNew:
		return "unpaired"
	default:
		return status
	}
}
