package diagnostics

import "errors"

// Reason is one stable, explicitly allowlisted message that may cross the
// owner-only local API boundary. Arbitrary operation errors are never
// published, even when they do not look like credentials.
type Reason string

const (
	ReasonCheckUnavailable         Reason = "diagnostic check is unavailable"
	ReasonCheckFailed              Reason = "diagnostic check failed"
	ReasonRecoveryUnavailable      Reason = "recovery operation is unavailable"
	ReasonRecoveryOperationFailed  Reason = "recovery operation failed"
	ReasonRecoveryNotConfirmed     Reason = "recovery action did not restore readiness"
	ReasonRemoteConnectionNotReady Reason = "remote connection is not ready"
	ReasonSSHIdentityNotReady      Reason = "pinned SSH identity is not ready"
	ReasonWSLNotRunning            Reason = "managed WSL distribution is not running"
	ReasonSystemdTargetNotReady    Reason = "managed systemd target is not ready"
	ReasonDockerSocketNotReady     Reason = "Docker socket is not ready"
	ReasonDiskUnavailable          Reason = "managed environment disk is not ready"
	ReasonSyncthingNotReady        Reason = "Syncthing connection is not ready"
	ReasonPortRelaysNotReady       Reason = "port relays are not ready"
	ReasonHostUnreachable         Reason = "paired host is unreachable"
	ReasonLANBlocked              Reason = "private LAN access is blocked"
	ReasonTunnelIdentityMismatch  Reason = "tunnel identity does not match paired trust"
	ReasonPeerBusy                Reason = "paired host already has an active client"
	ReasonWSLUnavailable          Reason = "managed WSL service is unavailable"
	ReasonLocalPortOccupied       Reason = "required local tunnel port is occupied"
)

var allowedReasons = map[Reason]struct{}{
	ReasonCheckUnavailable:         {},
	ReasonCheckFailed:              {},
	ReasonRecoveryUnavailable:      {},
	ReasonRecoveryOperationFailed:  {},
	ReasonRecoveryNotConfirmed:     {},
	ReasonRemoteConnectionNotReady: {},
	ReasonSSHIdentityNotReady:      {},
	ReasonWSLNotRunning:            {},
	ReasonSystemdTargetNotReady:    {},
	ReasonDockerSocketNotReady:     {},
	ReasonDiskUnavailable:          {},
	ReasonSyncthingNotReady:        {},
	ReasonPortRelaysNotReady:       {},
	ReasonHostUnreachable:          {},
	ReasonLANBlocked:               {},
	ReasonTunnelIdentityMismatch:   {},
	ReasonPeerBusy:                 {},
	ReasonWSLUnavailable:           {},
	ReasonLocalPortOccupied:        {},
}

type publicReasonError struct {
	reason Reason
}

func (e publicReasonError) Error() string { return string(e.reason) }

// NewPublicError marks an allowlisted reason as safe for publication. Unknown
// Reason values remain untrusted and are replaced by the caller's fallback.
func NewPublicError(reason Reason) error {
	return publicReasonError{reason: reason}
}

// ReasonForError returns an explicitly allowlisted reason or the supplied
// stable fallback. It deliberately does not inspect or redact err.Error().
func ReasonForError(err error, fallback Reason) string {
	if err == nil {
		return ""
	}
	var public publicReasonError
	if errors.As(err, &public) {
		if _, ok := allowedReasons[public.reason]; ok {
			return string(public.reason)
		}
	}
	if _, ok := allowedReasons[fallback]; !ok {
		fallback = ReasonCheckFailed
	}
	return string(fallback)
}
