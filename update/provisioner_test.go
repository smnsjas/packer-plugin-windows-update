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

func TestPrepareDefaults(t *testing.T) {
	t.Parallel()
	p := &Provisioner{}
	if err := p.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if p.config.RestartTimeout != 4*time.Hour {
		t.Errorf("RestartTimeout = %v; want %v", p.config.RestartTimeout, 4*time.Hour)
	}
	if p.config.Username != "SYSTEM" {
		t.Errorf("Username = %q; want %q", p.config.Username, "SYSTEM")
	}
	if p.config.SearchCriteria != "BrowseOnly=0 and IsInstalled=0" {
		t.Errorf("SearchCriteria = %q; want %q", p.config.SearchCriteria, "BrowseOnly=0 and IsInstalled=0")
	}
	if p.config.UpdateLimit != 1000 {
		t.Errorf("UpdateLimit = %d; want 1000", p.config.UpdateLimit)
	}
	if p.config.UpdateMaxRetries != 5 {
		t.Errorf("UpdateMaxRetries = %d; want 5", p.config.UpdateMaxRetries)
	}
}

func TestFiltersArgument(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		filters []string
		want    string
	}{
		{
			name:    "nil filters",
			filters: nil,
			want:    "",
		},
		{
			name:    "empty filters",
			filters: []string{},
			want:    "",
		},
		{
			name:    "single filter",
			filters: []string{"include:$true"},
			want:    " -Filters 'include:$true'",
		},
		{
			name:    "multiple filters",
			filters: []string{"exclude:$_.Title -like '*Preview*'", "include:$true"},
			want:    " -Filters 'exclude:$_.Title -like ''*Preview*''','include:$true'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := filtersArgument(tt.filters)
			if got != tt.want {
				t.Errorf("filtersArgument(%v) = %q; want %q", tt.filters, got, tt.want)
			}
		})
	}
}

func TestSearchCriteriaArgument(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		criteria string
		want     string
	}{
		{
			name:     "empty criteria",
			criteria: "",
			want:     "",
		},
		{
			name:     "recommended criteria",
			criteria: "BrowseOnly=0 and IsInstalled=0",
			want:     " -SearchCriteria 'BrowseOnly=0 and IsInstalled=0'",
		},
		{
			name:     "criteria with single quote",
			criteria: "Type='Software' and IsInstalled=0",
			want:     " -SearchCriteria 'Type=''Software'' and IsInstalled=0'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := searchCriteriaArgument(tt.criteria)
			if got != tt.want {
				t.Errorf("searchCriteriaArgument(%q) = %q; want %q", tt.criteria, got, tt.want)
			}
		})
	}
}

func TestEscapePowerShellString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no quotes",
			input: "hello",
			want:  "'hello'",
		},
		{
			name:  "single quote",
			input: "it's",
			want:  "'it''s'",
		},
		{
			name:  "multiple single quotes",
			input: "a'b'c",
			want:  "'a''b''c'",
		},
		{
			name:  "empty string",
			input: "",
			want:  "''",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := escapePowerShellString(tt.input)
			if got != tt.want {
				t.Errorf("escapePowerShellString(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}
