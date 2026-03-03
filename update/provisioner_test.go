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
			exitStatus:  exitCodeReboot,
			wantRestart: true,
			wantErr:     false,
		},
		{
			name:        "windows 2012 reboot required",
			exitStatus:  exitCodeWin2012Reboot,
			wantRestart: true,
			wantErr:     false,
		},
		{
			name:        "loop detected",
			exitStatus:  exitCodeLoop,
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

func TestPrepareDefaults(t *testing.T) {
	t.Parallel()
	p := &Provisioner{}
	if err := p.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if p.config.RestartTimeout != 4*time.Hour {
		t.Errorf("RestartTimeout = %v; want %v", p.config.RestartTimeout, 4*time.Hour)
	}
	if p.config.SearchCriteria != "BrowseOnly=0 and IsInstalled=0" {
		t.Errorf("SearchCriteria = %q; want %q", p.config.SearchCriteria, "BrowseOnly=0 and IsInstalled=0")
	}
	if p.config.UpdateLimit != 1000 {
		t.Errorf("UpdateLimit = %d; want 1000", p.config.UpdateLimit)
	}
	if p.config.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d; want 5", p.config.MaxRetries)
	}
}

func TestPrepareCustomValues(t *testing.T) {
	t.Parallel()
	p := &Provisioner{}
	err := p.Prepare(map[string]interface{}{
		"restart_timeout": "2h",
		"search_criteria": "IsInstalled=0",
		"filters":         []string{"exclude:$_.Title -like '*Preview*'"},
		"update_limit":    50,
		"max_retries":     3,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	if p.config.RestartTimeout != 2*time.Hour {
		t.Errorf("RestartTimeout = %v; want %v", p.config.RestartTimeout, 2*time.Hour)
	}
	if p.config.SearchCriteria != "IsInstalled=0" {
		t.Errorf("SearchCriteria = %q; want %q", p.config.SearchCriteria, "IsInstalled=0")
	}
	if len(p.config.Filters) != 1 || p.config.Filters[0] != "exclude:$_.Title -like '*Preview*'" {
		t.Errorf("Filters = %v; want [exclude:$_.Title -like '*Preview*']", p.config.Filters)
	}
	if p.config.UpdateLimit != 50 {
		t.Errorf("UpdateLimit = %d; want 50", p.config.UpdateLimit)
	}
	if p.config.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d; want 3", p.config.MaxRetries)
	}
}

func TestWindowsUpdateCommand(t *testing.T) {
	t.Parallel()
	p := &Provisioner{}
	p.config.SearchCriteria = "BrowseOnly=0 and IsInstalled=0"
	p.config.Filters = []string{"include:$true"}
	p.config.UpdateLimit = 25
	p.updateRunID = "run-123"

	command := p.windowsUpdateCommand()

	expectedParts := []string{
		"& 'C:/Windows/Temp/packer-windows-update.ps1'",
		"-SearchCriteria 'BrowseOnly=0 and IsInstalled=0'",
		"-Filters 'include:$true'",
		"-UpdateLimit 25",
		"-UpdateRunID 'run-123'",
	}

	for _, part := range expectedParts {
		if !strings.Contains(command, part) {
			t.Errorf("expected command to contain %q, got: %s", part, command)
		}
	}
}

func TestWindowsUpdateCommandNoFilters(t *testing.T) {
	t.Parallel()
	p := &Provisioner{}
	p.config.SearchCriteria = "BrowseOnly=0 and IsInstalled=0"
	p.config.UpdateLimit = 1000
	p.updateRunID = "run-456"

	command := p.windowsUpdateCommand()

	if strings.Contains(command, "-Filters") {
		t.Errorf("expected command to omit -Filters when none configured, got: %s", command)
	}
}

func TestWindowsUpdateCheckForRebootRequiredCommand(t *testing.T) {
	t.Parallel()
	p := &Provisioner{}

	command := p.windowsUpdateCheckForRebootRequiredCommand()

	expected := "& 'C:/Windows/Temp/packer-windows-update.ps1' -OnlyCheckForRebootRequired"
	if command != expected {
		t.Errorf("got %q; want %q", command, expected)
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
