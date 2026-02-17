package update

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestUpdateExitStatusError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		exitStatus  int
		wantRestart bool
		wantErr     bool
		errContains string
	}{
		{
			name:        "success",
			exitStatus:  0,
			wantRestart: false,
			wantErr:     false,
		},
		{
			name:        "reboot required",
			exitStatus:  windowsUpdateExitCodeReboot,
			wantRestart: true,
			wantErr:     false,
		},
		{
			name:        "windows 2012 reboot required",
			exitStatus:  2147942501,
			wantRestart: true,
			wantErr:     false,
		},
		{
			name:        "loop detected",
			exitStatus:  windowsUpdateExitCodeLoop,
			wantRestart: false,
			wantErr:     true,
			errContains: "repeated update loop",
		},
		{
			name:        "generic non-zero",
			exitStatus:  42,
			wantRestart: false,
			wantErr:     true,
			errContains: "non-zero exit status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			restartPending := false
			err := updateExitStatusError(tt.exitStatus, &restartPending)

			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
			if tt.wantErr && tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
				t.Fatalf("expected error containing %q, got %q", tt.errContains, err.Error())
			}
			if restartPending != tt.wantRestart {
				t.Fatalf("expected restartPending=%v, got %v", tt.wantRestart, restartPending)
			}
		})
	}
}

func TestWindowsUpdateCommandIncludesUpdateRunID(t *testing.T) {
	t.Parallel()
	p := &Provisioner{}
	p.config.UpdateLimit = 25
	p.updateRunID = "run-123"

	command := p.windowsUpdateCommand()
	if !strings.Contains(command, "-UpdateRunID") {
		t.Fatalf("expected command to include -UpdateRunID, got: %s", command)
	}
	if !strings.Contains(command, "run-123") {
		t.Fatalf("expected command to include run id value, got: %s", command)
	}
}

func TestWaitRetryDelayCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := waitRetryDelay(ctx, time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected cancellation error, got nil")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("expected quick return on cancellation, took %s", elapsed)
	}
}
