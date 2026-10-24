package collectiveobservations

import (
	"context"

	"github.com/peasant-labs/peasant/internal/auth"
)

// CredentialBackend supplies local profile selection and generation validation.
// Store transactions independently guard the actual credential snapshot.
type CredentialBackend interface {
	Capture(context.Context, string) (auth.CredentialSnapshot, error)
	CheckSnapshot(context.Context, auth.CredentialSnapshot) error
}

type diskCredentialBackend struct{}

var _ CredentialBackend = diskCredentialBackend{}

func (diskCredentialBackend) Capture(ctx context.Context, home string) (auth.CredentialSnapshot, error) {
	return auth.CaptureCredentials(ctx, home)
}

func (diskCredentialBackend) CheckSnapshot(ctx context.Context, snapshot auth.CredentialSnapshot) error {
	return snapshot.WithCurrent(ctx, func() error { return nil })
}
