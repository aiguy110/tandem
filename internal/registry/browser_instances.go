package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/eventlog"
)

// BrowserInstancesEvent is the durable session event carrying a session's full
// browser instance list. The newest one is authoritative: the daemon reloads
// it after a restart and the UI projects it from the transcript.
const BrowserInstancesEvent = "browser_instances"

type browserInstanceList struct {
	Kind      string             `json:"kind,omitempty"`
	Instances []browser.Instance `json:"instances"`
	// Next is the ordinal of the next additional instance. IDs are never
	// reused within a session, so a stale ID cannot alias a newer browser.
	Next int `json:"next"`
}

// instanceList returns the session's additional (non-default) browser
// instances, loading them from the event log on first use. r.instMu must be
// held.
func (r *Registry) instanceList(sessionID string) *browserInstanceList {
	if list := r.browserInstances[sessionID]; list != nil {
		return list
	}
	list := &browserInstanceList{Next: 2}
	if event, err := r.store.LatestEventOfKind(sessionID, BrowserInstancesEvent); err != nil {
		slog.Warn("load browser instances failed", "session_id", sessionID, "error", err)
	} else if event != nil {
		var stored browserInstanceList
		if err := json.Unmarshal([]byte(event.Payload), &stored); err != nil {
			slog.Warn("decode browser instances failed", "session_id", sessionID, "error", err)
		} else {
			for _, instance := range stored.Instances {
				if instance.ID == browser.DefaultBrowserID {
					source := instance
					r.defaultSources[sessionID] = &source
					continue
				}
				if browser.ValidBrowserID(instance.ID) {
					list.Instances = append(list.Instances, instance)
				}
			}
			if stored.Next > list.Next {
				list.Next = stored.Next
			}
		}
	}
	r.browserInstances[sessionID] = list
	return list
}

// defaultInstance describes the browser every session starts with, seeded
// from the spawn-time saved profile when there was one.
func (r *Registry) defaultInstance(sessionID string) browser.Instance {
	instance := browser.Instance{ID: browser.DefaultBrowserID, Name: "Default"}
	if s := r.Get(sessionID); s != nil && s.Spec.Profile != nil && s.Spec.Profile.Snapshot != "" {
		instance.SourceProfileID = s.Spec.Profile.Snapshot
		if snap, err := r.store.BrowserSnapshot(instance.SourceProfileID); err == nil && snap != nil {
			instance.SourceProfileName = snap.Name
		}
	}
	return instance
}

// ListInstances returns the default browser followed by every approved
// additional browser of a live session.
func (r *Registry) ListInstances(sessionID string) ([]browser.Instance, error) {
	if r.Get(sessionID) == nil {
		return nil, fmt.Errorf("no such agent: %s", sessionID)
	}
	defaultInstance := r.defaultInstance(sessionID)
	r.instMu.Lock()
	defer r.instMu.Unlock()
	list := r.instanceList(sessionID)
	if override := r.defaultSources[sessionID]; override != nil {
		defaultInstance.SourceProfileID, defaultInstance.SourceProfileName = override.SourceProfileID, override.SourceProfileName
	}
	return append([]browser.Instance{defaultInstance}, list.Instances...), nil
}

// HasBrowserInstance reports whether browserID names an existing browser of
// sessionID.
func (r *Registry) HasBrowserInstance(sessionID, browserID string) bool {
	if browserID == "" || browserID == browser.DefaultBrowserID {
		return r.Get(sessionID) != nil
	}
	r.instMu.Lock()
	defer r.instMu.Unlock()
	for _, instance := range r.instanceList(sessionID).Instances {
		if instance.ID == browserID {
			return true
		}
	}
	return false
}

// AuthorizeBrowserKey is the broker's provisioning gate. Default browsers keep
// their existing lazy behavior; an additional instance may only launch while
// it is in its live session's approved list.
func (r *Registry) AuthorizeBrowserKey(key string) error {
	sessionID, browserID := browser.SplitInstanceKey(key)
	if browserID == browser.DefaultBrowserID {
		return nil
	}
	if r.Get(sessionID) == nil || !r.HasBrowserInstance(sessionID, browserID) {
		return fmt.Errorf("browser %s is not an approved instance of this agent", browserID)
	}
	return nil
}

// ListSavedProfiles describes saved browser profiles without revealing where
// their state is stored.
func (r *Registry) ListSavedProfiles() ([]browser.SavedProfileInfo, error) {
	snaps, err := r.store.ListBrowserSnapshots()
	if err != nil {
		return nil, err
	}
	driver := ""
	if r.browser != nil {
		driver = r.browser.DriverKind()
	}
	out := make([]browser.SavedProfileInfo, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, browser.SavedProfileInfo{ID: snap.ID, Name: snap.Name, CreatedAt: snap.CreatedAt, Compatible: snap.Kind == "" || snap.Kind == driver})
	}
	return out, nil
}

// OpenInstance launches an additional browser for a session. It must only be
// called once a human approved the request; the caller enforces that.
func (r *Registry) OpenInstance(ctx context.Context, sessionID, profileID, name string) (browser.Instance, error) {
	s := r.Get(sessionID)
	if s == nil {
		return browser.Instance{}, fmt.Errorf("no such agent: %s", sessionID)
	}
	if r.browser == nil {
		return browser.Instance{}, errors.New("browser subsystem disabled")
	}
	var kind, ref, profileName string
	if profileID != "" {
		snap, err := r.store.BrowserSnapshot(profileID)
		if err != nil {
			return browser.Instance{}, err
		}
		if snap == nil {
			return browser.Instance{}, fmt.Errorf("no such saved browser profile: %s", profileID)
		}
		if snap.Kind != "" && snap.Kind != r.browser.DriverKind() {
			return browser.Instance{}, fmt.Errorf("saved browser profile %q is for the %s driver, not %s", snap.Name, snap.Kind, r.browser.DriverKind())
		}
		kind, ref, profileName = snap.Kind, snap.Ref, snap.Name
	}
	r.instMu.Lock()
	list := r.instanceList(sessionID)
	instance := browser.Instance{
		ID:              "b" + strconv.Itoa(list.Next),
		SourceProfileID: profileID, SourceProfileName: profileName,
		CreatedAt: time.Now().UnixMilli(),
	}
	list.Next++
	instance.Name = uniqueInstanceName(list.Instances, firstNonEmpty(strings.TrimSpace(name), profileName, "Fresh browser"))
	list.Instances = append(list.Instances, instance)
	r.persistInstancesLocked(s.ID, list)
	r.instMu.Unlock()

	key := browser.InstanceKey(sessionID, instance.ID)
	// A never-reused ID has no prior profile, but clear any leftover directory
	// from a crash so the working copy starts exactly from its source.
	_ = r.browser.Teardown(ctx, key)
	r.browser.SeedSnapshot(key, kind, ref)
	if err := r.browser.EnsureProvisioned(ctx, key); err != nil {
		slog.Warn("browser instance launch failed", "session_id", sessionID, "browser_id", instance.ID, "saved_profile_id", profileID, "error", err)
		r.removeInstance(sessionID, instance.ID)
		_ = r.browser.Teardown(context.Background(), key)
		return browser.Instance{}, fmt.Errorf("launch browser: %w", err)
	}
	slog.Info("browser instance opened", "session_id", sessionID, "browser_id", instance.ID, "saved_profile_id", profileID)
	return instance, nil
}

// CloseInstance ends an additional browser and discards its working copy.
func (r *Registry) CloseInstance(ctx context.Context, sessionID, browserID string) error {
	if browserID == "" || browserID == browser.DefaultBrowserID {
		return errors.New("the default browser cannot be closed; restart it instead")
	}
	if !r.removeInstance(sessionID, browserID) {
		return fmt.Errorf("no such browser instance: %s", browserID)
	}
	if r.browser != nil {
		if err := r.browser.Teardown(ctx, browser.InstanceKey(sessionID, browserID)); err != nil {
			slog.Warn("browser instance teardown failed", "session_id", sessionID, "browser_id", browserID, "error", err)
		}
	}
	slog.Info("browser instance closed", "session_id", sessionID, "browser_id", browserID)
	return nil
}

func (r *Registry) removeInstance(sessionID, browserID string) bool {
	r.instMu.Lock()
	defer r.instMu.Unlock()
	list := r.instanceList(sessionID)
	for i, instance := range list.Instances {
		if instance.ID == browserID {
			list.Instances = append(list.Instances[:i:i], list.Instances[i+1:]...)
			r.persistInstancesLocked(sessionID, list)
			return true
		}
	}
	return false
}

// setInstanceSource records the saved profile a browser was restarted from.
func (r *Registry) setInstanceSource(sessionID, browserID, profileID, profileName string) {
	r.instMu.Lock()
	defer r.instMu.Unlock()
	list := r.instanceList(sessionID)
	if browserID == browser.DefaultBrowserID {
		r.defaultSources[sessionID] = &browser.Instance{SourceProfileID: profileID, SourceProfileName: profileName}
	} else {
		for i := range list.Instances {
			if list.Instances[i].ID == browserID {
				list.Instances[i].SourceProfileID, list.Instances[i].SourceProfileName = profileID, profileName
			}
		}
	}
	r.persistInstancesLocked(sessionID, list)
}

// teardownInstances ends every additional browser of a closing session.
func (r *Registry) teardownInstances(ctx context.Context, sessionID string) {
	r.instMu.Lock()
	list := r.instanceList(sessionID)
	instances := append([]browser.Instance(nil), list.Instances...)
	delete(r.browserInstances, sessionID)
	delete(r.defaultSources, sessionID)
	r.instMu.Unlock()
	if r.browser == nil {
		return
	}
	for _, instance := range instances {
		_ = r.browser.Teardown(ctx, browser.InstanceKey(sessionID, instance.ID))
	}
}

// persistInstancesLocked appends the full instance list to the session's event
// log. r.instMu must be held.
func (r *Registry) persistInstancesLocked(sessionID string, list *browserInstanceList) {
	s := r.Get(sessionID)
	if s == nil {
		return
	}
	defaultInstance := r.defaultInstance(sessionID)
	if override := r.defaultSources[sessionID]; override != nil {
		defaultInstance.SourceProfileID, defaultInstance.SourceProfileName = override.SourceProfileID, override.SourceProfileName
	}
	payload, err := json.Marshal(browserInstanceList{Kind: BrowserInstancesEvent, Instances: append([]browser.Instance{defaultInstance}, list.Instances...), Next: list.Next})
	if err != nil {
		return
	}
	if _, err := s.AppendEvent(eventlog.Event{Kind: BrowserInstancesEvent, Payload: payload}); err != nil {
		slog.Warn("persist browser instances failed", "session_id", sessionID, "error", err)
	}
}

func uniqueInstanceName(existing []browser.Instance, name string) string {
	taken := make(map[string]bool, len(existing))
	for _, instance := range existing {
		taken[instance.Name] = true
	}
	if !taken[name] {
		return name
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s (%d)", name, n)
		if !taken[candidate] {
			return candidate
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
