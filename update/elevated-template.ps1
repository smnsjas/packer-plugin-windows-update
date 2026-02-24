Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$name = "{{.TaskName}}"
$f = $null
$logStream = $null
trap {
    Write-Output "ERROR: $_"
    Write-Output (($_.ScriptStackTrace -split '\r?\n') -replace '^(.*)$', 'ERROR: $1')
    Write-Output (($_.Exception.ToString() -split '\r?\n') -replace '^(.*)$', 'ERROR EXCEPTION: $1')
    if ($null -ne $logStream) { try { $logStream.Dispose() } catch {} }
    if ($null -ne $f) { try { $f.DeleteTask("\$name", 0) } catch {} }
    Exit 1
}
$log = "$env:SystemRoot\Temp\$name.out"
$s = New-Object -ComObject "Schedule.Service"
$s.Connect()
$t = $s.NewTask($null)
$t.XmlText = @'
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
    <RegistrationInfo>
        <Description>{{.TaskDescription}}</Description>
    </RegistrationInfo>
    <Principals>
        <Principal id="Author">
            <RunLevel>HighestAvailable</RunLevel>
        </Principal>
    </Principals>
    <Settings>
        <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
        <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
        <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
        <AllowHardTerminate>true</AllowHardTerminate>
        <StartWhenAvailable>false</StartWhenAvailable>
        <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
        <IdleSettings>
            <StopOnIdleEnd>false</StopOnIdleEnd>
            <RestartOnIdle>false</RestartOnIdle>
        </IdleSettings>
        <AllowStartOnDemand>true</AllowStartOnDemand>
        <Enabled>true</Enabled>
        <Hidden>false</Hidden>
        <RunOnlyIfIdle>false</RunOnlyIfIdle>
        <WakeToRun>false</WakeToRun>
        <ExecutionTimeLimit>PT24H</ExecutionTimeLimit>
        <Priority>4</Priority>
    </Settings>
    <Actions Context="Author">
        <Exec>
            <Command>powershell.exe</Command>
            <Arguments>-ExecutionPolicy Bypass -NoProfile -NonInteractive -Command "Start-Transcript -Path '%SYSTEMROOT%\Temp\{{.TaskName}}.out' -Force; try { &amp; { {{.Command}} } } finally { Stop-Transcript }"</Arguments>
        </Exec>
    </Actions>
</Task>
'@
$username = "{{.Username}}"
$password = "{{.Password}}"
if (!$password) {
    $password = $null
}
$f = $s.GetFolder("\")
$f.RegisterTaskDefinition($name, $t, 6, $username, $password, 1, $null) | Out-Null
$t = $f.GetTask("\$name")
$t.Run($null) | Out-Null
$timeout = 10
$sec = 0
while ((!($t.state -eq 4)) -and ($sec -lt $timeout)) {
    Start-Sleep -Seconds 1
    $sec++
}
if ($t.state -ne 4) {
    Write-Output "Warning: scheduled task '$name' did not reach running state within $timeout seconds (current state: $($t.state)). Waiting for completion anyway."
}
# Windows PowerShell 2 on Windows 7 does not have Get-CimInstance.
# PowerShell 6 does not have Get-WmiObject.
if (!(Get-Command Get-CimInstance -ErrorAction:SilentlyContinue)) {
    function Get-CimInstance {
        [CmdletBinding()]
        param(
            [Parameter(Mandatory = $True, Position = 0)]
            [string]
            $ClassName
        )
        Get-WmiObject -Class $ClassName
    }
}
$reportProgressInterval = New-TimeSpan -Minutes 1
$startDate = Get-Date
do {
    Start-Sleep -Seconds 5
    if (Test-Path $log) {
        if ($null -eq $logStream) {
            # Open with ReadWrite share so the writing process is not blocked.
            $logStream = [System.IO.StreamReader]::new(
                [System.IO.File]::Open($log, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite),
                [System.Text.Encoding]::Default,
                $true)
        }
        while (-not $logStream.EndOfStream) {
            Write-Output $logStream.ReadLine()
        }
    }
    $currentDate = Get-Date
    if ($currentDate.Subtract($startDate) -ge $reportProgressInterval) {
        $startDate = $currentDate
        Write-Output "Waiting for operation to complete..."
    }
} while (!($t.state -eq 3))
# Final drain: read any output written after the last poll cycle.
if ($null -ne $logStream) {
    while (-not $logStream.EndOfStream) {
        Write-Output $logStream.ReadLine()
    }
    $logStream.Dispose()
    $logStream = $null
}
elseif (Test-Path $log) {
    # Task completed before the log was opened (very fast execution).
    Get-Content $log
}
$result = $t.LastTaskResult
if (Test-Path $log) {
    if ($result -ne 0) {
        Start-Sleep -Milliseconds 500
        Write-Output ""
        Write-Output "--- TASK FAILED WITH EXIT CODE $result ---"
        Write-Output "--- FULL LOG DUMP TO ENSURE EXCEPTION VISIBILITY ---"
        Get-Content $log
        Write-Output "----------------------------------------------------"
    }
    Remove-Item $log -Force -ErrorAction SilentlyContinue | Out-Null
}

# delete scheduled task
$f.DeleteTask("\$name", 0)

[System.Runtime.Interopservices.Marshal]::ReleaseComObject($s) | Out-Null
exit $result
