# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased] - 2026-02-17

### Added

- Package-level and symbol-level doc comments across `update/provisioner.go`, `update/update_ui.go`, and `update/elevated.go`.
- Debug logging via `log.Printf` at provision start and each update attempt, visible with `PACKER_LOG=1`.
- Named constant `windowsUpdateExitCodeWin2012Reboot = 2147942501` replacing bare magic number.
- `t.Parallel()` on all tests and subtests in `update/provisioner_test.go`.
- Curated WUA HRESULT mapping for install result diagnostics in `update/windows-update.ps1`.
- Per-update install result logging including `ResultCode`, `HResult`, and reboot requirement.
- Install queue ordering that prioritizes servicing stack updates and exclusive updates before regular updates.
- Search and download bounded retry loops with backoff in `update/windows-update.ps1`.
- Update-loop detection with dedicated script exit status `102`.
- Per-run loop-state file isolation via `-UpdateRunID` passed from `update/provisioner.go` to `update/windows-update.ps1`.
- Targeted unit tests in `update/provisioner_test.go` for exit status handling, command construction, and cancellation-aware retry delay behavior.

### Changed

- Removed stdout-corrupting `fmt.Printf` calls in `Provision()` (stdout is the Packer gRPC protocol stream).
- Replaced error format verbs `%s` with `%w` for proper error wrapping compatible with `errors.Is`/`errors.As`.
- Lowercased error strings to follow Go conventions.
- Replaced O(n²) log-tailing in `update/elevated-template.ps1` (`Get-Content | Select-Object -skip`) with a `System.IO.StreamReader` opened with `FileShare.ReadWrite`, reading only new bytes per poll cycle.
- Simplified `searchCriteriaArgument` and `filtersArgument` helpers: replaced `bytes.Buffer` with direct string concatenation and `strings.Join`.
- Fixed `filtersArgument` nil-check to use `len(filters) == 0` covering both nil and empty slices.
- Pre-initialized `$name`, `$f`, and `$logStream` variables before the `trap` block in `update/elevated-template.ps1` to satisfy `Set-StrictMode -Version Latest` when the trap fires before full initialization.
- Removed dead unreachable username validation block in `Prepare()`.
- Improved PowerShell object construction and collection operations for efficiency (`[PSCustomObject]`, `[void]` collection adds).
- Replaced fragile terminal-error string matching with explicit terminal-error state handling.
- Added structured startup logging for run ID and loop-state path.
- Updated Go update flow to fail fast on exit `102` and use context-aware retry delays.
- Standardized reboot exit-status constant usage in Go update/restart status handling.

### Fixed

- Orphaned scheduled task in `update/elevated-template.ps1` when an error occurred after task registration but before `DeleteTask`; trap block now calls `DeleteTask` and disposes the log `StreamReader` on failure.
- Corrected HRESULT hashtable key typing to ensure lookup works with unsigned HRESULT values.
- Added loop-state cleanup on terminal script exits while preserving state across reboot-required exits.

### Documentation

- Updated `README.md` with updater exit-status semantics and a pointer to this changelog.
