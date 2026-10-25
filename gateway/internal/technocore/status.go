package technocore

import (
	"os"
	"sync"
	"time"

	tc "piso/internal/technocore"
)

// Status is a snapshot for the local dashboard.
type Status struct {
	Enrolled     bool       `json:"enrolled"`
	Status       string     `json:"status"`
	Name         string     `json:"name"`
	ServerURL    string     `json:"serverUrl"`
	Fingerprint  string     `json:"fingerprint"`
	PrincipalID  string     `json:"principalId,omitempty"`
	Placement    string     `json:"placement,omitempty"`
	PublicURL    string     `json:"publicUrl,omitempty"`
	PairURL      string     `json:"pairUrl,omitempty"`
	PairExpires  string     `json:"pairExpiresAt,omitempty"`
	PairPhase    string     `json:"pairPhase,omitempty"`
	PairApprover string     `json:"pairApprover,omitempty"`
	PairAccount  string     `json:"pairAccount,omitempty"`
	Awaiting     bool       `json:"awaitingConfirm"`
	LastSync     *time.Time `json:"lastSync,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	OfflineSince *time.Time `json:"offlineSince,omitempty"`
	EnvelopeSkip int        `json:"envelopeSkip"`
	Secrets      Counts     `json:"secrets"`
}

// Counts is the vault summary the dashboard shows after enrollment. It is the
// proof that pairing did something: a gateway that paired and synced nothing
// looks identical to one that never connected.
type Counts struct {
	Published int `json:"published"`
	Pending   int `json:"pending"`
	Inbound   int `json:"inbound"`
	Total     int `json:"total"`
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
			out.LastSync = nil
			out.OfflineSince = nil
			return out
		}
		out.LastError = err.Error()
		out.Status = "error"
		return out
	}
	out.Enrolled = id.Status == tc.StatusActive
	out.Status = displayStatus(id)
	out.Name = id.Name
	out.ServerURL = id.ServerURL
	out.Fingerprint = id.Fingerprint()
	out.PrincipalID = id.PrincipalID
	out.Placement = id.PlacementOrLocal()
	out.PublicURL = id.PublicURL
	if id.Status == tc.StatusPending && id.PairingID != "" {
		out.PairURL = tc.PairURL(id)
		out.PairExpires = id.PairExpiresAt
		out.PairPhase = id.PairPhase
		out.PairApprover = id.PairApprover
		out.PairAccount = id.PairAccount
		// A phone has approved; only the dashboard's own confirm can finish.
		out.Awaiting = id.PairPhase == tc.PairStatusAwaitingGateway
	} else {
		out.PairURL = ""
		out.PairExpires = ""
		out.PairPhase = ""
		out.PairApprover = ""
		out.PairAccount = ""
		out.Awaiting = false
	}
	if !out.Enrolled {
		// A sync only happens once this gateway is enrolled. Reporting a time
		// here otherwise renders as "synced 739905d ago" in the dashboard.
		out.LastSync = nil
		out.OfflineSince = nil
	}
	return out
}

// displayStatus maps a freshly generated identity to the same chip label as
// a missing file, and surfaces the second pairing phase that the coordinator
// reported. Enrollment still requires active.
func displayStatus(id tc.Identity) string {
	switch id.Status {
	case "", tc.StatusNew:
		return "unpaired"
	case tc.StatusPending:
		if id.PairPhase == tc.PairStatusAwaitingGateway {
			return "awaiting_gateway"
		}
		return tc.StatusPending
	default:
		return id.Status
	}
}

// ResetPairing clears a dead pairing attempt so the dashboard offers
// "Show pairing code" again. It is deliberately narrow: identity status and
// the pair fields only.
func ResetPairing(dataDir string) error {
	_, err := tc.Update(tc.Path(dataDir), func(cur *tc.Identity) error {
		if cur.Status == tc.StatusActive {
			return ErrAlreadyPaired
		}
		cur.Status = tc.StatusNew
		cur.PairingID = ""
		cur.PollToken = ""
		cur.PairPhase = ""
		cur.PairExpiresAt = ""
		cur.PairApprover = ""
		cur.PairAccount = ""
		return nil
	})
	return err
}
