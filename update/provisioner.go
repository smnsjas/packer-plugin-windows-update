//go:generate packer-sdc mapstructure-to-hcl2 -type Config

// Package update provides a Packer provisioner that installs Windows Updates on the guest machine.
// It drives the Windows Update Agent (WUA) via an embedded PowerShell script, handling
// automatic reboots and retries until no further updates are available.
//
// Based on https://github.com/hashicorp/packer/blob/81522dced0b25084a824e79efda02483b12dc7cd/provisioner/windows-restart/provisioner.go
package update

import (
	"bytes"
	"context"
	_ "embed" // this is needed for using the go:embed directive
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/retry"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
	"github.com/hashicorp/packer-plugin-sdk/uuid"
)

const (
	elevatedPath                       = "C:/Windows/Temp/packer-windows-update-elevated.ps1"
	elevatedCommand                    = "PowerShell -ExecutionPolicy Bypass -OutputFormat Text -File C:/Windows/Temp/packer-windows-update-elevated.ps1"
	windowsUpdatePath                  = "C:/Windows/Temp/packer-windows-update.ps1"
	pendingRebootElevatedPath          = "C:/Windows/Temp/packer-windows-update-pending-reboot-elevated.ps1"
	pendingRebootElevatedCommand       = "PowerShell -ExecutionPolicy Bypass -OutputFormat Text -File C:/Windows/Temp/packer-windows-update-pending-reboot-elevated.ps1"
	restartCommand                     = "shutdown.exe -f -r -t 0 -c \"packer restart\""
	testRestartCommand                 = "shutdown.exe -f -r -t 60 -c \"packer restart test\""
	abortTestRestartCommand            = "shutdown.exe -a"
	windowsUpdateExitCodeReboot        = 101
	windowsUpdateExitCodeWin2012Reboot = 2147942501 // ERROR_PROCESS_ABORTED, returned by Windows Server 2012
	windowsUpdateExitCodeLoop          = 102
	retryableDelay                     = 5 * time.Second
	uploadTimeout                      = 5 * time.Minute
)

//go:embed windows-update.ps1
var windowsUpdatePs1 []byte

// Config holds the provisioner configuration decoded from the HCL2 template.
type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// The timeout for waiting for the machine to restart
	RestartTimeout time.Duration `mapstructure:"restart_timeout"`

	// Instructs the communicator to run the remote script as a
	// Windows scheduled task, effectively elevating the remote
	// user by impersonating a logged-in user.
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`

	// The updates search criteria.
	// See the IUpdateSearcher::Search method at https://docs.microsoft.com/en-us/windows/desktop/api/wuapi/nf-wuapi-iupdatesearcher-search.
	SearchCriteria string `mapstructure:"search_criteria"`

	// Filters the installed Windows updates. If no filter is
	// matched the update is NOT installed.
	Filters []string `mapstructure:"filters"`

	// Adds a limit to how many updates are installed at a time
	UpdateLimit int `mapstructure:"update_limit"`

	// Max times the provisioner will try install the updates
	// in case of failure.
	UpdateMaxRetries int `mapstructure:"update_max_retries"`

	ctx interpolate.Context
}

// Provisioner is a Packer provisioner that installs Windows updates on the guest machine.
// It uploads an embedded PowerShell script and drives the Windows Update Agent (WUA),
// handling reboots and retries automatically.
type Provisioner struct {
	config      Config
	updateRunID string
}

func (b *Provisioner) ConfigSpec() hcldec.ObjectSpec {
	return b.config.FlatMapstructure().HCL2Spec()
}

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

	if p.config.Username == "" {
		p.config.Username = "SYSTEM"
	}

	if p.config.SearchCriteria == "" {
		p.config.SearchCriteria = "BrowseOnly=0 and IsInstalled=0"
	}

	if p.config.UpdateLimit == 0 {
		p.config.UpdateLimit = 1000
	}

	if p.config.UpdateMaxRetries == 0 {
		p.config.UpdateMaxRetries = 5
	}

	return nil
}

func (p *Provisioner) Provision(ctx context.Context, ui packer.Ui, comm packer.Communicator, _ map[string]interface{}) error {
	p.updateRunID = uuid.TimeOrderedUUID()
	log.Printf("[DEBUG] windows-update: starting provision, runID=%s", p.updateRunID)

	ui.Say("Uploading the Windows update elevated script...")
	var buffer bytes.Buffer
	err := elevatedTemplate.Execute(&buffer, elevatedOptions{
		Username:        p.config.Username,
		Password:        p.config.Password,
		TaskDescription: "Packer Windows update elevated task",
		TaskName:        fmt.Sprintf("packer-windows-update-%s", uuid.TimeOrderedUUID()),
		Command:         p.windowsUpdateCommand(),
	})
	if err != nil {
		return fmt.Errorf("creating windows update elevated template: %w", err)
	}
	err = retry.Config{StartTimeout: uploadTimeout}.Run(ctx, func(context.Context) error {
		if err := comm.Upload(
			elevatedPath,
			bytes.NewReader(buffer.Bytes()),
			nil); err != nil {
			return fmt.Errorf("uploading the windows update elevated script: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	ui.Say("Uploading the Windows update check for reboot required elevated script...")
	buffer.Reset()
	err = elevatedTemplate.Execute(&buffer, elevatedOptions{
		Username:        p.config.Username,
		Password:        p.config.Password,
		TaskDescription: "Packer Windows update pending reboot elevated task",
		TaskName:        fmt.Sprintf("packer-windows-update-pending-reboot-%s", uuid.TimeOrderedUUID()),
		Command:         p.windowsUpdateCheckForRebootRequiredCommand(),
	})
	if err != nil {
		return fmt.Errorf("creating pending-reboot elevated template: %w", err)
	}
	err = retry.Config{StartTimeout: uploadTimeout}.Run(ctx, func(context.Context) error {
		if err := comm.Upload(
			pendingRebootElevatedPath,
			bytes.NewReader(buffer.Bytes()),
			nil); err != nil {
			return fmt.Errorf("uploading the windows update pending-reboot elevated script: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	ui.Say("Uploading the Windows update script...")
	err = retry.Config{StartTimeout: uploadTimeout}.Run(ctx, func(context.Context) error {
		if err := comm.Upload(
			windowsUpdatePath,
			bytes.NewReader(windowsUpdatePs1),
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

func (p *Provisioner) update(ctx context.Context, ui packer.Ui, comm packer.Communicator) (bool, error) {
	ui.Say("Running Windows update...")
	var restartPending bool
	for try := 1; try <= p.config.UpdateMaxRetries; try++ {
		log.Printf("[DEBUG] windows-update: update attempt %d/%d", try, p.config.UpdateMaxRetries)
		ui := NewUpdateUi(ui)
		cmd := &packer.RemoteCmd{Command: elevatedCommand}
		err := cmd.RunWithUi(ctx, comm, ui)
		if err != nil {
			if try == p.config.UpdateMaxRetries {
				return restartPending, err
			}

			if err := waitRetryDelay(ctx, retryableDelay); err != nil {
				return restartPending, err
			}
			continue
		}
		var exitStatus = cmd.ExitStatus()
		if !ui.finished {
			err = fmt.Errorf("windows update script did not finish (exit status: %d)", exitStatus)
			if try == p.config.UpdateMaxRetries {
				return restartPending, err
			}

			if err := waitRetryDelay(ctx, retryableDelay); err != nil {
				return restartPending, err
			}
			continue
		}

		updateErr := updateExitStatusError(exitStatus, &restartPending)
		if updateErr == nil {
			return restartPending, nil
		}

		if exitStatus == windowsUpdateExitCodeLoop {
			return restartPending, updateErr
		}

		if try == p.config.UpdateMaxRetries {
			return restartPending, updateErr
		}

		if err := waitRetryDelay(ctx, retryableDelay); err != nil {
			return restartPending, err
		}
	}

	return restartPending, fmt.Errorf("windows update failed after %d retries", p.config.UpdateMaxRetries)
}

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
			// wait for the machine to reboot.
			cmd := &packer.RemoteCmd{Command: testRestartCommand}
			err := cmd.RunWithUi(ctx, comm, ui)
			if err != nil {
				return err
			}
			exitStatus := cmd.ExitStatus()
			if exitStatus != 0 {
				return fmt.Errorf("machine not yet available (exit status %d)", exitStatus)
			}
			cmd = &packer.RemoteCmd{Command: abortTestRestartCommand}
			err = cmd.RunWithUi(ctx, comm, ui)

			return err
		})
		if err != nil {
			return err
		}

		ui.Say("Checking for pending restart...")
		err = p.retryable(ctx, func(ctx context.Context) error {
			ui := NewUpdateUi(ui)
			cmd := &packer.RemoteCmd{Command: pendingRebootElevatedCommand}
			if err := cmd.RunWithUi(ctx, comm, ui); err != nil {
				return err
			}
			if !ui.finished {
				return fmt.Errorf("windows update script did not finish")
			}

			exitStatus := cmd.ExitStatus()
			switch exitStatus {
			case 0:
				restartPending = false
			case windowsUpdateExitCodeReboot:
				restartPending = true
			case windowsUpdateExitCodeWin2012Reboot:
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

// retryable will retry the given function over and over until a
// non-error is returned, RestartTimeout expires, or ctx is
// cancelled.
func (p *Provisioner) retryable(ctx context.Context, f func(ctx context.Context) error) error {
	return retry.Config{
		RetryDelay:   func() time.Duration { return retryableDelay },
		StartTimeout: p.config.RestartTimeout,
	}.Run(ctx, f)
}

// waitRetryDelay pauses for the given delay, returning ctx.Err() immediately if
// the context is cancelled before the timer fires.
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

// updateExitStatusError maps a Windows update PowerShell exit status to a Go error,
// setting *restartPending when the exit code indicates a pending reboot.
// Returns nil on success (exit status 0 or reboot-pending variants).
func updateExitStatusError(exitStatus int, restartPending *bool) error {
	switch exitStatus {
	case 0:
		return nil
	case windowsUpdateExitCodeReboot:
		*restartPending = true
		return nil
	case windowsUpdateExitCodeWin2012Reboot:
		*restartPending = true
		return nil
	case windowsUpdateExitCodeLoop:
		return fmt.Errorf("windows update script detected a repeated update loop (exit status: %d)", exitStatus)
	default:
		return fmt.Errorf("windows update script exited with non-zero exit status: %d", exitStatus)
	}
}

func (p *Provisioner) windowsUpdateCommand() string {
	return fmt.Sprintf(
		"PowerShell -ExecutionPolicy Bypass -OutputFormat Text -EncodedCommand %s",
		base64.StdEncoding.EncodeToString(
			encodeUtf16Le(fmt.Sprintf(
				"%s%s%s -UpdateLimit %d -UpdateRunID %s",
				windowsUpdatePath,
				searchCriteriaArgument(p.config.SearchCriteria),
				filtersArgument(p.config.Filters),
				p.config.UpdateLimit,
				escapePowerShellString(p.updateRunID)))))
}

func (p *Provisioner) windowsUpdateCheckForRebootRequiredCommand() string {
	return fmt.Sprintf(
		"PowerShell -ExecutionPolicy Bypass -OutputFormat Text -EncodedCommand %s",
		base64.StdEncoding.EncodeToString(
			encodeUtf16Le(fmt.Sprintf(
				"%s -OnlyCheckForRebootRequired",
				windowsUpdatePath))))
}

func encodeUtf16Le(s string) []byte {
	d := utf16.Encode([]rune(s))
	b := make([]byte, len(d)*2)
	for i, r := range d {
		b[i*2] = byte(r)
		b[i*2+1] = byte(r >> 8)
	}
	return b
}

func searchCriteriaArgument(searchCriteria string) string {
	if searchCriteria == "" {
		return ""
	}
	return " -SearchCriteria " + escapePowerShellString(searchCriteria)
}

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

func escapePowerShellString(value string) string {
	return fmt.Sprintf(
		"'%s'",
		// escape single quotes with another single quote.
		strings.ReplaceAll(value, "'", "''"))
}
