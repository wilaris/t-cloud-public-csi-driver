package evs_test

import (
	"context"
	"errors"
	"testing"

	"wilaris.dev/t-cloud-public-csi-drive/internal/evs"
)

func TestAttachVolume_Validation(t *testing.T) {
	client := &evs.Client{}

	tests := []struct {
		name     string
		volumeID string
		serverID string
	}{
		{
			name:     "empty volumeID",
			volumeID: "",
			serverID: "server-123",
		},
		{
			name:     "empty serverID",
			volumeID: "vol-123",
			serverID: "",
		},
		{
			name:     "whitespace volumeID",
			volumeID: "   ",
			serverID: "server-123",
		},
		{
			name:     "whitespace serverID",
			volumeID: "vol-123",
			serverID: "   ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := client.AttachVolume(context.Background(), tc.volumeID, tc.serverID)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !errors.Is(err, evs.ErrInvalidArgument) {
				t.Errorf("expected ErrInvalidArgument, got: %v", err)
			}
		})
	}
}

func TestDetachVolume_Validation(t *testing.T) {
	client := &evs.Client{}

	tests := []struct {
		name     string
		volumeID string
		serverID string
	}{
		{
			name:     "empty volumeID",
			volumeID: "",
			serverID: "server-123",
		},
		{
			name:     "empty serverID",
			volumeID: "vol-123",
			serverID: "",
		},
		{
			name:     "whitespace volumeID",
			volumeID: "   ",
			serverID: "server-123",
		},
		{
			name:     "whitespace serverID",
			volumeID: "vol-123",
			serverID: "   ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := client.DetachVolume(context.Background(), tc.volumeID, tc.serverID)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !errors.Is(err, evs.ErrInvalidArgument) {
				t.Errorf("expected ErrInvalidArgument, got: %v", err)
			}
		})
	}
}
