//go:generate packer-sdc mapstructure-to-hcl2 -type Config

// Package update provides a Packer provisioner that installs Windows Updates on the guest machine.
// It drives the Windows Update Agent (WUA) via an embedded PowerShell script, handling
// automatic reboots and retries until no further updates are available.
//
// This provisioner is communicator-agnostic: it uses the packer.Communicator interface
// and does not rely on elevated scheduled tasks or sentinel-based exit code detection.
package update

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/retry"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
	"github.com/hashicorp/packer-plugin-sdk/uuid"
)

const (
	windowsUpdatePath       = "C:/Windows/Temp/packer-windows-update.ps1"
	restartCommand          = `shutdown.exe -f -r -t 0 -c "packer restart"`
	testRestartCommand      = `shutdown.exe -f -r -t 60 -c "packer restart test"`
	abortTestRestartCommand = "shutdown.exe -a"
	exitCodeReboot          = 101
	exitCodeWin2012Reboot   = 2147942501 // ERROR_PROCESS_ABORTED, returned by Windows Server 2012
	exitCodeLoop            = 102
	retryableDelay          = 5 * time.Second
	uploadTimeout           = 5 * time.Minute
)

//go:embed windows_update.ps1
var windowsUpdateScript []byte

// Config holds the provisioner configuration decoded from the HCL2 template.
type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// RestartTimeout is the maximum time to wait for the machine to restart
	// and become available again. Defaults to 4 hours.
	RestartTimeout time.Duration `mapstructure:"restart_timeout"`

	// SearchCriteria is the Windows Update search criteria string passed to
	// IUpdateSearcher::Search. Defaults to "BrowseOnly=0 and IsInstalled=0".
	// See https://docs.microsoft.com/en-us/windows/desktop/api/wuapi/nf-wuapi-iupdatesearcher-search.
	SearchCriteria string `mapstructure:"search_criteria"`

	// Filters limits which updates are installed. Only updates matching at
	// least one filter expression are installed. When empty, all updates
	// matching SearchCriteria are installed.
	Filters []string `mapstructure:"filters"`

	// UpdateLimit caps the number of updates installed per invocation.
	// Defaults to 1000.
	UpdateLimit int `mapstructure:"update_limit"`

	// MaxRetries is the maximum number of attempts to install updates before
	// the provisioner gives up. Defaults to 5.
	MaxRetries int `mapstructure:"max_retries"`

	ctx interpolate.Context
}

// Provisioner is a Packer provisioner that installs Windows updates on the
// guest machine. It uploads an embedded PowerShell script and drives the
// Windows Update Agent (WUA), handling reboots and retries automatically.
type Provisioner struct {
	config      Config
	updateRunID string
}

// ConfigSpec returns the HCL2 object spec for the provisioner configuration.
func (p *Provisioner) ConfigSpec() hcldec.ObjectSpec {
	return p.config.FlatMapstructure().HCL2Spec()
}

// Prepare decodes the raw provisioner configuration and applies defaults.
func (p *Provisioner) Prepare(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"execute_command",
			},
		},
	}, raws...)
	if err != nil {
		return err
	}

	if p.config.RestartTimeout == 0 {
		p.config.RestartTimeout = 4 * time.Hour
	}

	if p.config.SearchCriteria == "" {
		p.config.SearchCriteria = "BrowseOnly=0 and IsInstalled=0"
	}

	if p.config.UpdateLimit == 0 {
		p.config.UpdateLimit = 1000
	}

	if p.config.MaxRetries == 0 {
		p.config.MaxRetries = 5
	}

	return nil
}

// Provision uploads the Windows update PowerShell script to the guest machine
// and runs it in a loop, rebooting automatically whenever the script signals
// that a restart is required.
func (p *Provisioner) Provision(ctx context.Context, ui packer.Ui, comm packer.Communicator, vars map[string]interface{}) error {
	_ = vars // generatedVars map is unused by this provisioner

	p.updateRunID = uuid.TimeOrderedUUID()
	log.Printf("[DEBUG] windows-update: starting provision, runID=%s", p.updateRunID)

	ui.Say("Uploading the Windows update script...")
	err := retry.Config{StartTimeout: uploadTimeout}.Run(ctx, func(context.Context) error {
		if err := comm.Upload(
			windowsUpdatePath,
			bytes.NewReader(windowsUpdateScript),
			nil); err != nil {
			return fmt.Errorf("uploading the windows update script: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	for {
		restartPending, err := p.update(ctx, ui, comm)
		if err != nil {
			return err
		}

		if !restartPending {
			return nil
		}

		err = p.restart(ctx, ui, comm)
		if err != nil {
			return err
		}
	}
}

// update runs the Windows update script with retries. It returns true when
// the script signals that a reboot is required and false when all updates
// have been installed successfully.
func (p *Provisioner) update(ctx context.Context, ui packer.Ui, comm packer.Communicator) (bool, error) {
	ui.Say("Running Windows update...")

	var restartPending bool

	for try := 1; try <= p.config.MaxRetries; try++ {
		log.Printf("[DEBUG] windows-update: update attempt %d/%d", try, p.config.MaxRetries)

		cmd := &packer.RemoteCmd{Command: p.windowsUpdateCommand()}
		err := cmd.RunWithUi(ctx, comm, ui)
		if err != nil {
			if try == p.config.MaxRetries {
				return restartPending, err
			}
			if waitErr := waitRetryDelay(ctx, retryableDelay); waitErr != nil {
				return restartPending, waitErr
			}
			continue
		}

		exitStatus := cmd.ExitStatus()
		log.Printf("[DEBUG] windows-update: update script exited with status %d", exitStatus)

		updateErr := updateExitStatusError(exitStatus, &restartPending)
		if updateErr == nil {
			return restartPending, nil
		}

		// A loop exit code means the script detected the same updates being
		// offered repeatedly; retrying will not help.
		if exitStatus == exitCodeLoop {
			return restartPending, updateErr
		}

		if try == p.config.MaxRetries {
			return restartPending, updateErr
		}

		if waitErr := waitRetryDelay(ctx, retryableDelay); waitErr != nil {
			return restartPending, waitErr
		}
	}

	return restartPending, fmt.Errorf("windows update failed after %d retries", p.config.MaxRetries)
}

// restart reboots the guest machine and waits for it to become available
// again. If a reboot is still pending after the machine comes back (detected
// via the script's -OnlyCheckForRebootRequired flag), the cycle repeats.
func (p *Provisioner) restart(ctx context.Context, ui packer.Ui, comm packer.Communicator) error {
	restartPending := true

	for restartPending {
		ui.Say("Restarting the machine...")
		err := p.retryable(ctx, func(ctx context.Context) error {
			cmd := &packer.RemoteCmd{Command: restartCommand}
			err := cmd.RunWithUi(ctx, comm, ui)
			if err != nil {
				return err
			}
			exitStatus := cmd.ExitStatus()
			if exitStatus != 0 {
				return fmt.Errorf("failed to restart the machine with exit status: %d", exitStatus)
			}
			return nil
		})
		if err != nil {
			return err
		}

		ui.Say("Waiting for machine to become available...")
		err = p.retryable(ctx, func(ctx context.Context) error {
			// Schedule a restart with a 60-second delay to test connectivity.
			cmd := &packer.RemoteCmd{Command: testRestartCommand}
			err := cmd.RunWithUi(ctx, comm, ui)
			if err != nil {
				return err
			}
			exitStatus := cmd.ExitStatus()
			if exitStatus != 0 {
				return fmt.Errorf("machine not yet available (exit status %d)", exitStatus)
			}

			// Cancel the test restart now that we know the machine is up.
			cmd = &packer.RemoteCmd{Command: abortTestRestartCommand}
			err = cmd.RunWithUi(ctx, comm, ui)
			return err
		})
		if err != nil {
			return err
		}

		ui.Say("Checking for pending restart...")
		err = p.retryable(ctx, func(ctx context.Context) error {
			cmd := &packer.RemoteCmd{Command: p.windowsUpdateCheckForRebootRequiredCommand()}
			if err := cmd.RunWithUi(ctx, comm, ui); err != nil {
				return err
			}

			exitStatus := cmd.ExitStatus()
			switch exitStatus {
			case 0:
				restartPending = false
			case exitCodeReboot:
				restartPending = true
			case exitCodeWin2012Reboot:
				restartPending = true
			default:
				return fmt.Errorf("machine not yet available (exit status %d)", exitStatus)
			}

			return nil
		})
		if err != nil {
			return err
		}

		if restartPending {
			ui.Say("Restart is still pending...")
		} else {
			ui.Say("Restart complete")
		}
	}

	return nil
}

// retryable retries the given function until it succeeds, RestartTimeout
// expires, or ctx is cancelled. Retries are separated by retryableDelay.
func (p *Provisioner) retryable(ctx context.Context, f func(ctx context.Context) error) error {
	return retry.Config{
		RetryDelay:   func() time.Duration { return retryableDelay },
		StartTimeout: p.config.RestartTimeout,
	}.Run(ctx, f)
}

// windowsUpdateCommand builds the PowerShell invocation that runs the
// embedded update script with the configured search criteria, filters,
// update limit, and run ID.
func (p *Provisioner) windowsUpdateCommand() string {
	return fmt.Sprintf(
		"& %s%s%s -UpdateLimit %d -UpdateRunID %s",
		escapePowerShellString(windowsUpdatePath),
		searchCriteriaArgument(p.config.SearchCriteria),
		filtersArgument(p.config.Filters),
		p.config.UpdateLimit,
		escapePowerShellString(p.updateRunID))
}

// windowsUpdateCheckForRebootRequiredCommand builds the PowerShell invocation
// that checks whether a reboot is still pending after the machine restarts.
func (p *Provisioner) windowsUpdateCheckForRebootRequiredCommand() string {
	return fmt.Sprintf(
		"& %s -OnlyCheckForRebootRequired",
		escapePowerShellString(windowsUpdatePath))
}

// waitRetryDelay pauses for the given delay, returning ctx.Err() immediately
// if the context is cancelled before the timer fires.
func waitRetryDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// updateExitStatusError maps a Windows update PowerShell exit status to a Go
// error, setting *restartPending when the exit code indicates a pending reboot.
// Returns nil on success (exit status 0 or reboot-pending variants).
func updateExitStatusError(exitStatus int, restartPending *bool) error {
	switch exitStatus {
	case 0:
		return nil
	case exitCodeReboot:
		*restartPending = true
		return nil
	case exitCodeWin2012Reboot:
		*restartPending = true
		return nil
	case exitCodeLoop:
		return fmt.Errorf("windows update script detected a repeated update loop (exit status: %d)", exitStatus)
	default:
		return fmt.Errorf("windows update script exited with non-zero exit status: %d", exitStatus)
	}
}

// searchCriteriaArgument formats the -SearchCriteria flag for the PowerShell
// script invocation. Returns an empty string when criteria is empty.
func searchCriteriaArgument(criteria string) string {
	if criteria == "" {
		return ""
	}
	return " -SearchCriteria " + escapePowerShellString(criteria)
}

// filtersArgument formats the -Filters flag for the PowerShell script
// invocation. Returns an empty string when no filters are configured.
func filtersArgument(filters []string) string {
	if len(filters) == 0 {
		return ""
	}
	escaped := make([]string, len(filters))
	for i, filter := range filters {
		escaped[i] = escapePowerShellString(filter)
	}
	return " -Filters " + strings.Join(escaped, ",")
}

// escapePowerShellString wraps a value in single quotes and escapes any
// embedded single quotes by doubling them, following PowerShell conventions.
func escapePowerShellString(value string) string {
	return fmt.Sprintf(
		"'%s'",
		strings.ReplaceAll(value, "'", "''"))
}
