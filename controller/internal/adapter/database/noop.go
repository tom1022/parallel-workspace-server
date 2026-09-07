package database

import "context"

// NoopAdapter provisions nothing: deployments without a database backend
// select this instead of CNPGAdapter (Requirement 3.5), and a workspace is
// built without one (resources.go reads WorkspaceTemplateSpec.Database being
// nil to reach the same outcome per template).
type NoopAdapter struct{}

var _ DatabaseAdapter = (*NoopAdapter)(nil)

func (NoopAdapter) Ensure(context.Context, DatabaseTarget) (DatabaseRef, error) {
	return DatabaseRef{}, nil
}

func (NoopAdapter) Release(context.Context, DatabaseTarget) error {
	return nil
}
