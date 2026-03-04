# see Using the Windows Update Agent API | Searching, Downloading, and Installing Updates
#     at https://msdn.microsoft.com/en-us/library/windows/desktop/aa387102(v=vs.85).aspx
# see ISystemInformation interface
#     at https://msdn.microsoft.com/en-us/library/windows/desktop/aa386095(v=vs.85).aspx
# see IUpdateSession interface
#     at https://msdn.microsoft.com/en-us/library/windows/desktop/aa386854(v=vs.85).aspx
# see IUpdateSearcher interface
#     at https://msdn.microsoft.com/en-us/library/windows/desktop/aa386515(v=vs.85).aspx
# see IUpdateSearcher::Search method
#     at https://docs.microsoft.com/en-us/windows/desktop/api/wuapi/nf-wuapi-iupdatesearcher-search
# see IUpdateDownloader interface
#     at https://msdn.microsoft.com/en-us/library/windows/desktop/aa386131(v=vs.85).aspx
# see IUpdateCollection interface
#     at https://msdn.microsoft.com/en-us/library/windows/desktop/aa386107(v=vs.85).aspx
# see IUpdate interface
#     at https://msdn.microsoft.com/en-us/library/windows/desktop/aa386099(v=vs.85).aspx
# see xWindowsUpdateAgent DSC resource
#     at https://github.com/PowerShell/xWindowsUpdate/blob/dev/DscResources/MSFT_xWindowsUpdateAgent/MSFT_xWindowsUpdateAgent.psm1
# NB you can install common sets of updates with one of these settings:
#       | Name          | SearchCriteria                            | Filters       |
#       |---------------|-------------------------------------------|---------------|
#       | Important     | AutoSelectOnWebSites=1 and IsInstalled=0  | $true         |
#       | Recommended   | BrowseOnly=0 and IsInstalled=0            | $true         |
#       | All           | IsInstalled=0                             | $true         |
#       | Optional Only | AutoSelectOnWebSites=0 and IsInstalled=0  | $_.BrowseOnly |

param(
    [string]$SearchCriteria = 'BrowseOnly=0 and IsInstalled=0',
    [string[]]$Filters = @('include:$true'),
    [int]$UpdateLimit = 1000,
    [string]$UpdateRunID = '',
    [switch]$OnlyCheckForRebootRequired = $false
)

$mock = $false
$searchMaxRetries = 30
$downloadMaxRetries = 30
$retryBaseDelaySeconds = 5
$retryMaxDelaySeconds = 60
$updateLoopStatePath = "$env:SystemRoot\Temp\packer-windows-update-loop-state.json"
$updateLoopMaxConsecutiveRounds = 3
$exitCodeUpdateLoopDetected = 102

if ($UpdateRunID) {
    $safeUpdateRunID = $UpdateRunID -replace '[^a-zA-Z0-9._-]', '_'
    $updateLoopStatePath = "$env:SystemRoot\Temp\packer-windows-update-loop-state-$safeUpdateRunID.json"
}

function Write-LogInfo($message) {
    Write-Output "INFO: $message"
}

function Write-LogWarn($message) {
    Write-Output "WARN: $message"
}

function Write-LogError($message) {
    Write-Output "ERROR: $message"
}

function Get-RetryDelaySeconds($attempt) {
    $power = [Math]::Min($attempt - 1, 5)
    $delay = $retryBaseDelaySeconds * [Math]::Pow(2, $power)
    return [int][Math]::Min($delay, $retryMaxDelaySeconds)
}

function Get-UpdateIdentity($update) {
    try {
        if ($null -ne $update.Identity -and $update.Identity.UpdateID) {
            return [string]$update.Identity.UpdateID
        }
    }
    catch {
    }

    return ("title::{0}" -f $update.Title)
}

function Get-UpdateLoopState {
    if (!(Test-Path $updateLoopStatePath)) {
        return $null
    }

    try {
        return Get-Content -Raw $updateLoopStatePath | ConvertFrom-Json
    }
    catch {
        Write-LogWarn "Failed to read update loop state from '$updateLoopStatePath': $_"
        return $null
    }
}

function Set-UpdateLoopState($state) {
    [System.IO.File]::WriteAllText(
        $updateLoopStatePath,
        ($state | ConvertTo-Json -Compress -Depth 10),
        (New-Object System.Text.UTF8Encoding $false))
}

function Clear-UpdateLoopState {
    if (Test-Path $updateLoopStatePath) {
        Remove-Item -Force $updateLoopStatePath
    }
}

function ExitWithCode($exitCode) {
    if ($exitCode -ne 101) {
        try {
            Clear-UpdateLoopState
        }
        catch {
            Write-LogWarn "Failed to clear update loop state '$updateLoopStatePath': $_"
        }
    }

    exit $exitCode
}

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
trap {
    Write-LogError $_
    Write-Output (($_.ScriptStackTrace -split '\r?\n') -replace '^(.*)$', 'ERROR: $1')
    Write-Output (($_.Exception.ToString() -split '\r?\n') -replace '^(.*)$', 'ERROR EXCEPTION: $1')
    ExitWithCode 1
}

if ($UpdateRunID) {
    Write-LogInfo "Using update run id '$UpdateRunID' with loop state path '$updateLoopStatePath'."
}
else {
    Write-LogWarn "No update run id provided; using shared loop state path '$updateLoopStatePath'."
}

if ($mock) {
    $mockWindowsUpdatePath = 'C:\Windows\Temp\windows-update-count-mock.txt'
    if (!(Test-Path $mockWindowsUpdatePath)) {
        Set-Content $mockWindowsUpdatePath 10
    }
    $count = [int]::Parse((Get-Content $mockWindowsUpdatePath).Trim())
    if ($count) {
        Write-Output "Synthetic reboot countdown counter is at $count"
        Set-Content $mockWindowsUpdatePath (--$count)
        ExitWithCode 101
    }
    Write-Output 'No Windows updates found'
    ExitWithCode 0
}

Add-Type @'
using System;
using System.Runtime.InteropServices;

public static class Windows
{
    [DllImport("kernel32", SetLastError=true)]
    public static extern UInt64 GetTickCount64();

    public static TimeSpan GetUptime()
    {
        return TimeSpan.FromMilliseconds(GetTickCount64());
    }
}
'@

function Wait-Condition {
    param(
        [scriptblock]$Condition,
        [int]$DebounceSeconds = 15
    )
    process {
        $begin = [Windows]::GetUptime()
        do {
            Start-Sleep -Seconds 1
            try {
                $result = &$Condition
            }
            catch {
                $result = $false
            }
            if (-not $result) {
                $begin = [Windows]::GetUptime()
                continue
            }
        } while ((([Windows]::GetUptime()) - $begin).TotalSeconds -lt $DebounceSeconds)
    }
}

$operationResultCodes = @{
    0 = "NotStarted";
    1 = "InProgress";
    2 = "Succeeded";
    3 = "SucceededWithErrors";
    4 = "Failed";
    5 = "Aborted"
}

function LookupOperationResultCode($code) {
    if ($operationResultCodes.ContainsKey($code)) {
        return $operationResultCodes[$code]
    }
    return "Unknown Code $code"
}

$wuaHResultMessages = @{
    ([uint32]'0x00240005') = 'The system must be restarted to complete installation of the update (WU_S_REBOOT_REQUIRED)';
    ([uint32]'0x80240009') = 'Another conflicting operation was in progress (WU_E_OPERATIONINPROGRESS)';
    ([uint32]'0x80240016') = 'Install not allowed, likely due to pending restart or conflicting install (WU_E_INSTALL_NOT_ALLOWED)';
    ([uint32]'0x80240017') = 'Operation was not performed because there are no applicable updates (WU_E_NOT_APPLICABLE)';
    ([uint32]'0x80240019') = 'An exclusive update cannot be installed with other updates at the same time (WU_E_EXCLUSIVE_INSTALL_CONFLICT)';
    ([uint32]'0x8024001F') = 'Operation did not complete because the network connection was unavailable (WU_E_NO_CONNECTION)';
    ([uint32]'0x80240021') = 'Operation timed out (WU_E_TIME_OUT)';
    ([uint32]'0x80240022') = 'Operation failed for all the updates (WU_E_ALL_UPDATES_FAILED)';
    ([uint32]'0x80240032') = 'The search criteria string was invalid (WU_E_INVALID_CRITERIA)';
    ([uint32]'0x8024200D') = 'The update needs to be downloaded again (WU_E_UH_NEEDANOTHERDOWNLOAD)';
    ([uint32]'0x80242014') = 'The post-reboot operation for the update is still in progress (WU_E_UH_POSTREBOOTSTILLPENDING)';
    ([uint32]'0x80242017') = 'The servicing stack must be updated before this update can be installed (WU_E_UH_NEW_SERVICING_STACK_REQUIRED)';
    ([uint32]'0x8024201D') = 'The update handler is disabled until the system reboots (WU_E_UH_HANDLER_DISABLEDUNTILREBOOT)';
    ([uint32]'0x80244022') = 'The update service is temporarily overloaded (WU_E_PT_HTTP_STATUS_SERVICE_UNAVAIL)';
    ([uint32]'0x8024A007') = 'A reboot is in progress (WU_E_REBOOT_IN_PROGRESS)';
    ([uint32]'0x8024D00C') = 'Windows Update Agent requires a reboot to fix setup state (WU_E_SETUP_REBOOT_TO_FIX)';
    ([uint32]'0x8024D00E') = 'Windows Update Agent setup requires reboot to complete installation (WU_E_SETUP_REBOOTREQUIRED)';
    ([uint32]'0x80240024') = 'There are no registered WUA services (WU_E_NO_SERVICE)';
    ([uint32]'0x80240FFF') = 'An unexpected error occurred (WU_E_UNEXPECTED)';
    ([uint32]'0x80070005') = 'Access denied; the caller does not have sufficient permissions (E_ACCESSDENIED)';
    ([uint32]'0x8024402C') = 'Could not connect; DNS name could not be resolved (WU_E_PT_WINHTTP_NAME_NOT_RESOLVED)';
    ([uint32]'0x80244016') = 'Same as HTTP 500; server encountered an unexpected condition (WU_E_PT_HTTP_STATUS_SERVER_ERROR)'
}

$rebootRequiredHResults = @(
    [uint32]'0x00240005',
    [uint32]'0x80240016',
    [uint32]'0x80242014',
    [uint32]'0x8024201D',
    [uint32]'0x8024A007',
    [uint32]'0x8024D00C',
    [uint32]'0x8024D00E'
)

$servicingStackRequiredHResults = @(
    [uint32]'0x80242017'
)

function ConvertTo-UInt32HResult($hresult) {
    if ($null -eq $hresult) { return [uint32]0 }
    return [uint32]("0x{0:X8}" -f [int32]$hresult)
}

function LookupWuaHResultMessage($hresult) {
    $unsignedHResult = ConvertTo-UInt32HResult $hresult
    if ($wuaHResultMessages.ContainsKey($unsignedHResult)) {
        return $wuaHResultMessages[$unsignedHResult]
    }
    return ('Unknown WUA HRESULT 0x{0:X8}' -f $unsignedHResult)
}

function Test-HResultInSet($hresult, $set) {
    $unsignedHResult = ConvertTo-UInt32HResult $hresult
    return $set -contains $unsignedHResult
}

function ExitWhenRebootRequired($rebootRequired = $false) {
    # check for pending Windows Updates.
    if (!$rebootRequired) {
        $systemInformation = New-Object -ComObject 'Microsoft.Update.SystemInfo'
        $rebootRequired = $systemInformation.RebootRequired
    }

    # check for pending Windows Features.
    if (!$rebootRequired) {
        $pendingPackagesKey = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\PackagesPending'
        $pendingPackagesCount = (Get-ChildItem -ErrorAction SilentlyContinue $pendingPackagesKey | Measure-Object).Count
        $rebootRequired = $pendingPackagesCount -gt 0
    }

    if ($rebootRequired) {
        Write-Output 'Waiting for the Windows Modules Installer to exit...'
        Wait-Condition { (Get-Process -ErrorAction SilentlyContinue TiWorker | Measure-Object).Count -eq 0 }
        ExitWithCode 101
    }
}

# try to repair the windows update settings to work in non-preview mode.
# see https://github.com/rgl/packer-plugin-windows-update/issues/144
# see https://learn.microsoft.com/en-sg/answers/questions/1791668/powershell-command-outputting-system-comobject-on
function Repair-WindowsUpdate {
    $settingsPath = 'C:\ProgramData\Microsoft\Windows\OneSettings\UusSettings.json'
    if (!(Test-Path $settingsPath)) {
        throw 'the windows update api is in an invalid state. see https://github.com/rgl/packer-plugin-windows-update/issues/144.'
    }
    $version = (New-Object -ComObject Microsoft.Update.AgentInfo).GetInfo('ProductVersionString')
    $settings = Get-Content -Raw $settingsPath | ConvertFrom-Json
    if ($settings.settings.EXCLUSIONS -notcontains $version) {
        $settings.settings.EXCLUSIONS += $version
        Write-Output 'Repairing the windows update settings to work in non-preview mode...'
        Copy-Item $settingsPath "$settingsPath.backup.json" -Force
        [System.IO.File]::WriteAllText(
            $settingsPath,
            ($settings | ConvertTo-Json -Compress -Depth 100),
            (New-Object System.Text.UTF8Encoding $false))
    }
    Write-Output 'Restarting the machine to retry a new windows update round...'
    ExitWithCode 101
}

ExitWhenRebootRequired

if ($OnlyCheckForRebootRequired) {
    Write-Output "$env:COMPUTERNAME restarted."
    ExitWithCode 0
}

$updateFilters = $Filters | ForEach-Object {
    $action, $expression = $_ -split ':', 2
    [PSCustomObject]@{
        Action     = $action
        Expression = [ScriptBlock]::Create($expression)
    }
}

function Test-IncludeUpdate($filters, $update) {
    foreach ($filter in $filters) {
        if (Where-Object -InputObject $update $filter.Expression) {
            return $filter.Action -eq 'include'
        }
    }
    return $false
}

function Add-UpdatesToCollection($fromCollection, $toCollection) {
    for ($i = 0; $i -lt $fromCollection.Count; ++$i) {
        [void]$toCollection.Add($fromCollection.Item($i))
    }
}

# Rebuild a category collection to only include updates that downloaded successfully.
function Rebuild-DownloadedCollection($sourceCollection, $downloadSucceededIdentities) {
    $rebuilt = New-Object -ComObject 'Microsoft.Update.UpdateColl'
    for ($i = 0; $i -lt $sourceCollection.Count; ++$i) {
        $u = $sourceCollection.Item($i)
        if ($downloadSucceededIdentities.ContainsKey((Get-UpdateIdentity $u))) {
            [void]$rebuilt.Add($u)
        }
    }
    return $rebuilt
}

# Install a single phase of updates. Handles per-update result logging, reboot detection,
# servicing stack prerequisites, and exclusive install conflicts.
# Returns a hashtable: @{ RebootRequired = $bool; TerminalErrors = [List[string]] }
function Install-UpdatePhase {
    param(
        [string]$PhaseName,
        $UpdateCollection,
        $UpdateInstaller
    )

    $result = @{
        RebootRequired = $false
        TerminalErrors = New-Object System.Collections.Generic.List[string]
    }

    if ($UpdateCollection.Count -eq 0) {
        return $result
    }

    Write-Output "=== Phase: $PhaseName ($($UpdateCollection.Count) update(s)) ==="

    $UpdateInstaller.Updates = $UpdateCollection

    $servicingStackRequiredDetected = $false
    $isTerminalInstallError = $false
    try {
        # Use async BeginInstall so we can report which update is currently installing.
        $installJob = $UpdateInstaller.BeginInstall($null, $null, $null)
        $lastReportedPct = -1
        $lastReportedIdx = -1
        $lastReportTime = [DateTime]::UtcNow
        while (-not $installJob.IsCompleted) {
            Start-Sleep -Seconds 5
            try {
                $prog = $installJob.GetProgress()
                $pct = [int]$prog.PercentComplete
                $curIdx = [int]$prog.CurrentUpdateIndex
                $elapsed = ([DateTime]::UtcNow - $lastReportTime).TotalSeconds
                if ($curIdx -ne $lastReportedIdx -or ($pct - $lastReportedPct -ge 5) -or $elapsed -ge 30) {
                    $curTitle = if ($curIdx -ge 0 -and $curIdx -lt $UpdateCollection.Count) {
                        $UpdateCollection.Item($curIdx).Title
                    } else { '(completing...)' }
                    Write-Output ("  [$PhaseName] Installing [{0}/{1}]: '{2}' - {3}% overall" -f `
                        ($curIdx + 1), $UpdateCollection.Count, $curTitle, $pct)
                    $lastReportedPct = $pct
                    $lastReportedIdx = $curIdx
                    $lastReportTime = [DateTime]::UtcNow
                }
            } catch {}
        }
        $installResult = $UpdateInstaller.EndInstall($installJob)
        try { $installJob.CleanUp() } catch {}

        if ($installResult.RebootRequired) {
            $result.RebootRequired = $true
        }

        for ($i = 0; $i -lt $UpdateCollection.Count; ++$i) {
            $installedUpdate = $UpdateCollection.Item($i)
            $updateResult = $installResult.GetUpdateResult($i)
            $updateResultCode = LookupOperationResultCode($updateResult.ResultCode)
            $updateHResult = ConvertTo-UInt32HResult $updateResult.HResult
            $updateResultMessage = LookupWuaHResultMessage $updateHResult
            $updateRebootRequired = $updateResult.RebootRequired

            Write-Output (("[{0}] Install result: '{1}' => ResultCode={2} ({3}), HResult=0x{4:X8}, RebootRequired={5}, Message='{6}'" `
                ) -f $PhaseName, $installedUpdate.Title, $updateResult.ResultCode, $updateResultCode, $updateHResult, $updateRebootRequired, $updateResultMessage)

            if ($updateResult.ResultCode -eq 2) {
                continue
            }

            if ($updateRebootRequired -or (Test-HResultInSet $updateHResult $rebootRequiredHResults)) {
                $result.RebootRequired = $true
            }

            # WU_E_EXCLUSIVE_INSTALL_CONFLICT: the update needed exclusive access.
            # Treat as reboot-required so the Go loop retries after reboot rather than failing.
            if ((ConvertTo-UInt32HResult $updateHResult) -eq [uint32]'0x80240019') {
                Write-LogWarn "Exclusive install conflict for '$($installedUpdate.Title)'; will retry after reboot."
                $result.RebootRequired = $true
                continue
            }

            if (Test-HResultInSet $updateHResult $servicingStackRequiredHResults) {
                $servicingStackRequiredDetected = $true
                $result.RebootRequired = $true
                continue
            }

            $result.TerminalErrors.Add((
                    "'{0}' failed with ResultCode={1} ({2}), HResult=0x{3:X8} ({4})"
                ) -f $installedUpdate.Title, $updateResult.ResultCode, $updateResultCode, $updateHResult, $updateResultMessage)
        }

        if ($servicingStackRequiredDetected) {
            Write-Output "[$PhaseName] Servicing stack prerequisite detected. A reboot will be performed before retrying."
        }

        if ($result.TerminalErrors.Count -gt 0) {
            $isTerminalInstallError = $true
            throw ("[$PhaseName] Installation encountered non-reboot terminal errors: " + ($result.TerminalErrors -join '; '))
        }
    }
    catch {
        Write-Warning "[$PhaseName] Installation failed with error:"
        Write-Warning $_.Exception.ToString()

        if ($isTerminalInstallError) {
            throw
        }

        # Unknown failure -- request reboot and retry next round.
        $result.RebootRequired = $true
    }

    if ($result.RebootRequired) {
        Write-Output "=== Phase $PhaseName complete: reboot required ==="
    }
    else {
        Write-Output "=== Phase $PhaseName complete: no reboot required ==="
    }

    return $result
}

$windowsOsVersion = [System.Environment]::OSVersion.Version

Write-LogInfo 'Searching for Windows updates...'
$updatesToDownloadSize = 0
$updatesToDownload = New-Object -ComObject 'Microsoft.Update.UpdateColl'
# Five ordered installation categories:
#   1. Servicing Stack Updates (SSU) -- must install first to enable other updates
#   2. Cumulative / Monthly Rollup updates -- large OS-level patches
#   3. Exclusive updates (InstallationBehavior.Impact=2) -- require sole access
#   4. Regular updates -- everything else
#   5. Defender updates -- installed last to avoid interference with other updates
$updatesToInstallServicingStack = New-Object -ComObject 'Microsoft.Update.UpdateColl'
$updatesToInstallCumulative = New-Object -ComObject 'Microsoft.Update.UpdateColl'
$updatesToInstallExclusive = New-Object -ComObject 'Microsoft.Update.UpdateColl'
$updatesToInstallRegular = New-Object -ComObject 'Microsoft.Update.UpdateColl'
$updatesToInstallDefender = New-Object -ComObject 'Microsoft.Update.UpdateColl'
$queuedUpdateTitles = @{}
$searchResult = $null
for ($searchAttempt = 1; $searchAttempt -le $searchMaxRetries; ++$searchAttempt) {
    try {
        $updateSession = New-Object -ComObject 'Microsoft.Update.Session'
        $updateSession.ClientApplicationID = 'packer-windows-update'
        $updateSearcher = $updateSession.CreateUpdateSearcher()
        $searchResult = $updateSearcher.Search($SearchCriteria)
        if ($searchResult.ResultCode -eq 2) {
            break
        }
        $searchStatus = LookupOperationResultCode($searchResult.ResultCode)
    }
    catch {
        $searchStatus = $_.ToString()
    }
    if ($searchAttempt -eq $searchMaxRetries) {
        throw "Search for Windows updates failed after $searchAttempt attempts: $searchStatus"
    }

    $delaySeconds = Get-RetryDelaySeconds $searchAttempt
    Write-LogWarn "Search for Windows updates failed with '$searchStatus' (attempt $searchAttempt/$searchMaxRetries). Retrying in $delaySeconds seconds..."
    Start-Sleep -Seconds $delaySeconds
}

if ($null -eq $searchResult) {
    throw 'Search did not produce a valid result.'
}
$rebootRequired = $false
for ($i = 0; $i -lt $searchResult.Updates.Count; ++$i) {
    $update = $searchResult.Updates.Item($i)

    # when the windows update api returns an invalid update object, repair
    # windows update and signal a reboot to try again.
    # see https://github.com/rgl/packer-plugin-windows-update/issues/144
    # see The June 2024 preview update might impact applications using Windows Update APIs
    #     https://learn.microsoft.com/en-us/windows/release-health/status-windows-11-23h2#3351msgdesc
    $expectedProperties = @(
        'Title'
        'MaxDownloadSize'
        'LastDeploymentChangeTime'
        'InstallationBehavior'
        'AcceptEula'
    )
    $properties = $update `
    | Get-Member $expectedProperties `
    | Select-Object -ExpandProperty Name
    if (!$properties -or (Compare-Object $expectedProperties $properties)) {
        Repair-WindowsUpdate
    }

    $updateTitle = $update.Title
    $updateMaxDownloadSize = $update.MaxDownloadSize
    $updateDate = $update.LastDeploymentChangeTime.ToString('yyyy-MM-dd')
    $updateSize = ($updateMaxDownloadSize / 1024 / 1024).ToString('0.##')
    $updateSummary = "Windows update ($updateDate; $updateSize MB): $updateTitle"

    if (!(Test-IncludeUpdate $updateFilters $update)) {
        Write-Output "Skipped (filter) $updateSummary"
        continue
    }

    if ($update.InstallationBehavior.CanRequestUserInput) {
        Write-Output "Warning The update '$updateTitle' has the CanRequestUserInput flag set (if the install hangs, you might need to exclude it with the filter 'exclude:`$_.InstallationBehavior.CanRequestUserInput' or 'exclude:`$_.Title -like '*$updateTitle*'')"
    }

    if ($queuedUpdateTitles.ContainsKey($updateTitle)) {
        Write-Output "Warning, Skipping queueing the duplicated titled update '$updateTitle'."
        continue
    }

    Write-Output "Found $updateSummary"

    [void]$update.AcceptEula()

    $updatesToDownloadSize += $updateMaxDownloadSize
    [void]$updatesToDownload.Add($update)

    $isServicingStackUpdate = $updateTitle -match '(?i)\bservicing stack update\b|\bSSU\b'
    $isCumulativeUpdate = $updateTitle -match '(?i)\bcumulative update\b|\bmonthly.*rollup\b|\bmonthly quality rollup\b'
    $isDefenderUpdate = $updateTitle -match '(?i)\b(?:windows|microsoft)\s+defender\b|\bsecurity intelligence update\b|\bantimalware platform update\b'
    $isExclusiveUpdate = $update.InstallationBehavior.Impact -eq 2

    if ($isServicingStackUpdate) {
        Write-Output "Queued [servicing stack] $updateSummary"
        [void]$updatesToInstallServicingStack.Add($update)
    }
    elseif ($isDefenderUpdate) {
        Write-Output "Queued [defender] $updateSummary"
        [void]$updatesToInstallDefender.Add($update)
    }
    elseif ($isCumulativeUpdate) {
        Write-Output "Queued [cumulative] $updateSummary"
        [void]$updatesToInstallCumulative.Add($update)
    }
    elseif ($isExclusiveUpdate) {
        Write-Output "Queued [exclusive] $updateSummary"
        [void]$updatesToInstallExclusive.Add($update)
    }
    else {
        Write-Output "Queued [regular] $updateSummary"
        [void]$updatesToInstallRegular.Add($update)
    }
    $queuedUpdateTitles[$updateTitle] = $true

    $updatesToInstallCount = $updatesToInstallServicingStack.Count + $updatesToInstallCumulative.Count + $updatesToInstallExclusive.Count + $updatesToInstallRegular.Count + $updatesToInstallDefender.Count
    if ($updatesToInstallCount -ge $UpdateLimit) {
        $rebootRequired = $true
        break
    }
}

$updatesToInstall = New-Object -ComObject 'Microsoft.Update.UpdateColl'
Add-UpdatesToCollection $updatesToInstallServicingStack $updatesToInstall
Add-UpdatesToCollection $updatesToInstallCumulative $updatesToInstall
Add-UpdatesToCollection $updatesToInstallExclusive $updatesToInstall
Add-UpdatesToCollection $updatesToInstallRegular $updatesToInstall
Add-UpdatesToCollection $updatesToInstallDefender $updatesToInstall
if ($updatesToInstall.Count) {
    Write-Output ("Install order: {0} servicing stack, {1} cumulative, {2} exclusive, {3} regular, {4} defender" -f `
        $updatesToInstallServicingStack.Count, $updatesToInstallCumulative.Count, `
        $updatesToInstallExclusive.Count, $updatesToInstallRegular.Count, `
        $updatesToInstallDefender.Count)

    $queuedUpdateIdentities = New-Object System.Collections.Generic.List[string]
    for ($i = 0; $i -lt $updatesToInstall.Count; ++$i) {
        [void]$queuedUpdateIdentities.Add((Get-UpdateIdentity $updatesToInstall.Item($i)))
    }
    $queuedUpdateIdentities = @($queuedUpdateIdentities | Sort-Object -Unique)
    $queuedFingerprint = $queuedUpdateIdentities -join '|'

    $loopState = Get-UpdateLoopState
    $consecutiveLoopCount = 1
    if ($null -ne $loopState -and $loopState.Fingerprint -eq $queuedFingerprint) {
        $consecutiveLoopCount = [int]$loopState.ConsecutiveCount + 1
    }

    $newLoopState = [PSCustomObject]@{
        Fingerprint      = $queuedFingerprint
        ConsecutiveCount = $consecutiveLoopCount
        UpdateIdentities = $queuedUpdateIdentities
        LastUpdatedUtc   = [DateTime]::UtcNow.ToString('o')
    }
    Set-UpdateLoopState $newLoopState

    if ($consecutiveLoopCount -ge $updateLoopMaxConsecutiveRounds) {
        Write-LogError ("An update loop was detected after {0} consecutive rounds with the same updates: {1}" -f $consecutiveLoopCount, ($queuedUpdateIdentities -join ', '))
        ExitWithCode $exitCodeUpdateLoopDetected
    }
}

# Track how many updates failed to download so the post-install block can
# distinguish "nothing downloaded" from "search returned zero updates".
$downloadFailedCount = 0
if ($updatesToDownload.Count) {
    $updateSize = ($updatesToDownloadSize / 1024 / 1024).ToString('0.##')
    Write-Output "Downloading Windows updates ($($updatesToDownload.Count) updates; $updateSize MB)..."
    # https://docs.microsoft.com/en-us/windows/desktop/api/winnt/ns-winnt-_osversioninfoexa#remarks
    $downloadPriority = if (($windowsOsVersion.Major -eq 6 -and $windowsOsVersion.Minor -gt 1) -or ($windowsOsVersion.Major -gt 6)) {
        4 # 1 (dpLow), 2 (dpNormal), 3 (dpHigh), 4 (dpExtraHigh).
    }
    else {
        3 # For versions lower than 6.2 highest priority is 3.
    }

    # Download each update individually so that failures are isolated, retried separately,
    # and Packer output contains a progress line per update during the (potentially multi-hour)
    # download phase.  Batch downloading with a single Download() call was causing reboot loops:
    # a SucceededWithErrors result on the batch would trigger an unconditional reboot, and after
    # three identical rounds the loop-detection logic would abort the build.  Per-update downloads
    # let us inspect each update's HResult and only request a reboot when that specific update
    # requires one; updates that fail to download are left for the next round instead.
    $downloadSucceededIdentities = @{}
    for ($dlIdx = 0; $dlIdx -lt $updatesToDownload.Count; ++$dlIdx) {
        $dlUpdate = $updatesToDownload.Item($dlIdx)
        $dlTitle = $dlUpdate.Title
        $dlIdentity = Get-UpdateIdentity $dlUpdate
        $dlSize = ($dlUpdate.MaxDownloadSize / 1024 / 1024).ToString('0.##')

        # Skip downloading if the update content is already cached locally.
        if ($dlUpdate.IsDownloaded) {
            Write-Output "Already cached $($dlIdx + 1)/$($updatesToDownload.Count): $dlTitle ($dlSize MB)"
            $downloadSucceededIdentities[$dlIdentity] = $true
            continue
        }

        Write-Output "Downloading update $($dlIdx + 1)/$($updatesToDownload.Count): $dlTitle ($dlSize MB)..."

        $singleColl = New-Object -ComObject 'Microsoft.Update.UpdateColl'
        [void]$singleColl.Add($dlUpdate)
        $dl = $updateSession.CreateUpdateDownloader()
        $dl.Priority = $downloadPriority
        $dl.Updates = $singleColl

        # Resource check once before the retry loop (not on every retry).
        $os = Get-CimInstance Win32_OperatingSystem
        $disk = Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='C:'"
        $freeMemory = ($os.FreePhysicalMemory / 1024).ToString('0.0')
        $freeDisk = ($disk.FreeSpace / 1024 / 1024 / 1024).ToString('0.0')
        Write-Output "  Resources before download: Free Memory=$freeMemory MB, Free Disk=$freeDisk GB"

        $dlSucceeded = $false
        for ($dlAttempt = 1; $dlAttempt -le $downloadMaxRetries; ++$dlAttempt) {
            $dlResult = $null

            try {
                # Use async BeginDownload so we can poll and report progress.
                $dlJob = $dl.BeginDownload($null, $null, $null)
                $lastReportedPct = -1
                $lastReportTime = [DateTime]::UtcNow
                while (-not $dlJob.IsCompleted) {
                    Start-Sleep -Seconds 5
                    try {
                        $prog = $dlJob.GetProgress()
                        $pct = [int]$prog.PercentComplete
                        $elapsed = ([DateTime]::UtcNow - $lastReportTime).TotalSeconds
                        if ($pct -ne $lastReportedPct -and ($pct - $lastReportedPct -ge 5 -or $elapsed -ge 30)) {
                            $dlMB = ($dlUpdate.MaxDownloadSize / 1024 / 1024)
                            $doneMB = try { [double]$prog.TotalBytesDownloaded / 1024 / 1024 } catch { 0 }
                            if ($dlMB -gt 0 -and $doneMB -gt 0) {
                                Write-Output ("  Downloading: {0}% ({1:0.#} / {2:0.##} MB)" -f $pct, $doneMB, $dlMB)
                            } else {
                                Write-Output "  Downloading: $pct%"
                            }
                            $lastReportedPct = $pct
                            $lastReportTime = [DateTime]::UtcNow
                        }
                    } catch {}
                }
                $dlResult = $dl.EndDownload($dlJob)
                try { $dlJob.CleanUp() } catch {}
            }
            catch {
                if ($dlAttempt -eq $downloadMaxRetries) {
                    Write-LogWarn "Download of '$dlTitle' failed after $dlAttempt attempts: $_"
                    break
                }
                $delaySecs = Get-RetryDelaySeconds $dlAttempt
                Write-LogWarn "Download of '$dlTitle' threw an exception (attempt $dlAttempt/$downloadMaxRetries). Retrying in $delaySecs seconds..."
                Start-Sleep -Seconds $delaySecs
                continue
            }
            $perUpdateResult = $dlResult.GetUpdateResult(0)
            $perResultCode = $perUpdateResult.ResultCode
            $perHResult = ConvertTo-UInt32HResult $perUpdateResult.HResult

            if ($dlResult.ResultCode -eq 2 -and $perResultCode -eq 2) {
                Write-Output "Downloaded '$dlTitle' successfully."
                $dlSucceeded = $true
                break
            }

            # SucceededWithErrors: the download finished but with per-update issues.
            # Do NOT trigger a full reboot loop -- only reboot if the per-update HResult
            # explicitly requires it; otherwise log and skip to the next update.
            if ($dlResult.ResultCode -eq 3 -or $perResultCode -eq 3) {
                $hrMsg = LookupWuaHResultMessage $perHResult
                Write-LogWarn ("Download of '$dlTitle' succeeded with errors: HResult=0x{0:X8} ({1})" -f $perHResult, $hrMsg)
                if (Test-HResultInSet $perHResult $rebootRequiredHResults) {
                    Write-LogWarn "A pending reboot is required before '$dlTitle' can complete downloading."
                    $rebootRequired = $true
                }
                break  # Do not retry SucceededWithErrors; it will be retried next round.
            }

            if ($dlAttempt -eq $downloadMaxRetries) {
                $dlStatus = LookupOperationResultCode($dlResult.ResultCode)
                Write-LogWarn ("Download of '$dlTitle' failed after $dlAttempt attempts: ResultCode={0} ({1}), HResult=0x{2:X8}" -f
                    $perResultCode, $dlStatus, $perHResult)
                break
            }

            $dlStatus = LookupOperationResultCode($dlResult.ResultCode)
            $delaySecs = Get-RetryDelaySeconds $dlAttempt
            Write-LogWarn "Download of '$dlTitle' failed with $dlStatus (attempt $dlAttempt/$downloadMaxRetries). Retrying in $delaySecs seconds..."
            Start-Sleep -Seconds $delaySecs
        }

        if ($dlSucceeded) {
            $downloadSucceededIdentities[$dlIdentity] = $true
        }
    }

    # Rebuild each category collection to only include updates that downloaded successfully.
    # Updates that failed to download are left for the next round; the loop-detection
    # state machine will catch genuine infinite-download cycles.
    $updatesToInstallServicingStack = Rebuild-DownloadedCollection $updatesToInstallServicingStack $downloadSucceededIdentities
    $updatesToInstallCumulative = Rebuild-DownloadedCollection $updatesToInstallCumulative $downloadSucceededIdentities
    $updatesToInstallExclusive = Rebuild-DownloadedCollection $updatesToInstallExclusive $downloadSucceededIdentities
    $updatesToInstallRegular = Rebuild-DownloadedCollection $updatesToInstallRegular $downloadSucceededIdentities
    $updatesToInstallDefender = Rebuild-DownloadedCollection $updatesToInstallDefender $downloadSucceededIdentities

    $downloadFailedCount = $updatesToDownload.Count - $downloadSucceededIdentities.Count
    if ($downloadFailedCount -gt 0) {
        Write-LogWarn "$downloadFailedCount of $($updatesToDownload.Count) update(s) failed to download and will be retried in a subsequent round."
    }
}

$totalToInstall = $updatesToInstallServicingStack.Count + $updatesToInstallCumulative.Count + $updatesToInstallExclusive.Count + $updatesToInstallRegular.Count + $updatesToInstallDefender.Count
if ($totalToInstall -gt 0) {
    Write-Output "Installing $totalToInstall update(s) in phased order..."

    $updateInstaller = $updateSession.CreateUpdateInstaller()

    # Force Windows Installer-based updates to install without user interaction.
    # IUpdateInstaller2::ForceQuiet prevents MSI prompts from hanging an unattended build.
    # Some updates may not support this; WU_E_UH_DOESNOTSUPPORTACTION is non-fatal.
    try {
        $updateInstaller.ForceQuiet = $true
    }
    catch {
        Write-LogWarn "Could not set ForceQuiet on installer (may not be supported on this OS): $_"
    }

    # Install phases in strict order. After each phase, if a reboot is required we exit
    # immediately with code 101. The Go provisioner will reboot the VM and re-run the script;
    # on the next invocation the already-installed updates will not appear in search results,
    # so the script naturally advances to the next phase.
    $installPhases = @(
        @{ Name = 'Servicing Stack';  Collection = $updatesToInstallServicingStack },
        @{ Name = 'Cumulative';       Collection = $updatesToInstallCumulative },
        @{ Name = 'Exclusive';        Collection = $updatesToInstallExclusive },
        @{ Name = 'Regular';          Collection = $updatesToInstallRegular },
        @{ Name = 'Defender';         Collection = $updatesToInstallDefender }
    )

    foreach ($phase in $installPhases) {
        if ($phase.Collection.Count -eq 0) {
            continue
        }

        $phaseResult = Install-UpdatePhase -PhaseName $phase.Name -UpdateCollection $phase.Collection -UpdateInstaller $updateInstaller

        if ($phaseResult.RebootRequired -or $rebootRequired) {
            # Reboot before proceeding to the next phase. Remaining phases will run
            # in a subsequent invocation after the VM restarts.
            ExitWhenRebootRequired $true
        }
    }

    # All phases completed without requiring a reboot mid-way. Still check if
    # the system has a pending reboot from any phase.
    ExitWhenRebootRequired $rebootRequired
}
else {
    ExitWhenRebootRequired $rebootRequired
    if ($downloadFailedCount -gt 0) {
        Write-LogWarn "All $downloadFailedCount update(s) failed to download; they will be retried in the next round."
    }
    else {
        Write-Output 'No Windows updates found'
    }
}

ExitWithCode 0
