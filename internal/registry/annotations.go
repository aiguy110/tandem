package registry

import "github.com/aiguy110/tandem/internal/store"

// ListAnnotations returns an agent's pending transcript annotation tray.
func (r *Registry) ListAnnotations(sessionID string) ([]store.Annotation, error) {
	return r.store.ListAnnotations(sessionID)
}

// UpsertAnnotation creates or updates a transcript annotation.
func (r *Registry) UpsertAnnotation(a store.Annotation) error {
	return r.store.UpsertAnnotation(a)
}

// DeleteAnnotation removes one annotation.
func (r *Registry) DeleteAnnotation(id string) error {
	return r.store.DeleteAnnotation(id)
}

// ClearAnnotations removes every annotation for an agent (consumed on send),
// returning the count removed.
func (r *Registry) ClearAnnotations(sessionID string) (int, error) {
	return r.store.DeleteAnnotationsForSession(sessionID)
}
