package database

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"google.golang.org/protobuf/proto"
)

// EnvironmentRevisionSnapshot persists resolved composition alongside the base
// source identity. Session reservation verifies it against the requested pair.
type EnvironmentRevisionSnapshot struct {
	BaseRevision string                              `json:"baseRevision"`
	Environment  *apiv1alpha1.DevelopmentEnvironment `json:"environment"`
	Composition  *apiv1alpha1.RuntimeComposition     `json:"composition"`
	Provenance   json.RawMessage                     `json:"provenance"`
}

// LookupSessionCreation reads a creation request by authenticated creator and
// request ID. Missing requests return ErrNotFound; checkpoint forks cannot be
// reused as creation requests. It performs no preparation or writes.
func (c *Client) LookupSessionCreation(ctx context.Context, creator, requestID string) (*apiv1alpha1.Session, error) {
	row, err := readSessionRequest(ctx, c.db, creator, requestID)
	if err != nil {
		return nil, notFoundOr(err)
	}
	if row.SourceCheckpointID != nil {
		return nil, ErrIdempotencyConflict
	}
	return toSession(row)
}

// SameSessionSelection compares caller inputs, excluding mutable display names
// and operator defaults. An existing request keeps its original resolved payload.
func SameSessionSelection(session, request *apiv1alpha1.Session) bool {
	return proto.Equal(session.GetAgent(), request.GetAgent()) && proto.Equal(session.GetWorkspace(), request.GetWorkspace()) && proto.Equal(session.GetDevelopmentEnvironment(), request.GetDevelopmentEnvironment()) &&
		proto.Equal(&apiv1alpha1.Session{Credentials: session.GetCredentials()}, &apiv1alpha1.Session{Credentials: request.GetCredentials()})
}

func validateEnvironmentRevision(pinned runtimeRevisionRow, request *apiv1alpha1.Session, baseRevision string) error {
	if pinned.AgentName != request.GetAgent().GetName() || pinned.Namespace != request.GetAgent().GetNamespace() {
		return fmt.Errorf("selected revision belongs to a different Agent: %w", ErrConflict)
	}
	var snapshot EnvironmentRevisionSnapshot
	if err := json.Unmarshal(pinned.SourceSnapshot, &snapshot); err != nil {
		return fmt.Errorf("decode environment revision snapshot: %w", err)
	}
	if snapshot.BaseRevision != baseRevision || snapshot.BaseRevision == "" || snapshot.Environment == nil || snapshot.Composition == nil || !proto.Equal(snapshot.Environment, request.GetDevelopmentEnvironment()) || !proto.Equal(snapshot.Composition, request.GetRuntimeComposition()) {
		return fmt.Errorf("selected revision does not match resolved environment: %w", ErrConflict)
	}
	return nil
}

// inheritEnvironmentRevision restores output metadata from the exact checkpoint
// revision, preserving legacy provenance arrays and objects without composition.
func inheritEnvironmentRevision(raw []byte, session *apiv1alpha1.Session) error {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || data[0] != '{' {
		return nil
	}
	var snapshot EnvironmentRevisionSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode fork environment: %w", err)
	}
	if snapshot.BaseRevision == "" && snapshot.Environment == nil && snapshot.Composition == nil {
		return nil
	}
	if snapshot.BaseRevision == "" || snapshot.Environment == nil || snapshot.Composition == nil {
		return fmt.Errorf("fork revision has incomplete environment composition")
	}
	session.DevelopmentEnvironment = snapshot.Environment
	session.RuntimeComposition = snapshot.Composition
	return nil
}
