package browser

import (
	"regexp"
	"strings"
)

// DefaultBrowserID names the browser instance every agent session starts with.
// Its broker key is the bare session ID, so single-browser callers (automation
// runs, older UIs) keep working unchanged.
const DefaultBrowserID = "default"

// instanceKeySep joins a session ID and an additional browser instance ID into
// one broker key. The result is used as a URL path segment (escaped) and as a
// Chromium user-data-dir name, so it must be filesystem-safe.
const instanceKeySep = "~"

var browserIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidBrowserID reports whether id is a well-formed browser instance ID.
func ValidBrowserID(id string) bool { return browserIDPattern.MatchString(id) }

// InstanceKey returns the broker key for one browser instance of a session.
func InstanceKey(sessionID, browserID string) string {
	if browserID == "" || browserID == DefaultBrowserID {
		return sessionID
	}
	return sessionID + instanceKeySep + browserID
}

// SplitInstanceKey reverses InstanceKey.
func SplitInstanceKey(key string) (sessionID, browserID string) {
	if i := strings.LastIndex(key, instanceKeySep); i > 0 && ValidBrowserID(key[i+1:]) {
		return key[:i], key[i+1:]
	}
	return key, DefaultBrowserID
}

// Instance is one browser an agent session can drive. Every session has the
// default instance; additional instances exist only after a human approved
// them, each with its own Chromium profile, tabs and control owner.
type Instance struct {
	ID string `json:"id"`
	// Name is shown to the human and the agent. It defaults to the source saved
	// profile's name.
	Name string `json:"name"`
	// SourceProfileID/Name identify the saved browser profile the instance was
	// started from; empty means it started fresh. The working copy never writes
	// back to its source.
	SourceProfileID   string `json:"sourceProfileId,omitempty"`
	SourceProfileName string `json:"sourceProfileName,omitempty"`
	CreatedAt         int64  `json:"createdAt,omitempty"`
}

// SavedProfileInfo is the agent-visible description of a saved browser
// profile. It deliberately omits the on-disk location of the stored state.
type SavedProfileInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"createdAt"`
	// Compatible is false when the profile was captured by a different browser
	// driver and cannot be restored here.
	Compatible bool `json:"compatible"`
}
