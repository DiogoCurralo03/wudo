# Diagnoses why the wudo logon task is missing and, when it can, registers it.
#
# Run it elevated. The easiest way, with the service active, is:
#
#   wudo powershell -NoProfile -ExecutionPolicy Bypass -File C:\wudo\bin\diagnose-task.ps1
#
# It writes a full report to %TEMP%\wudo-diag.txt and prints the important parts.
#
# Everything here is read-only except the "REPAIR" section, which targets the same
# task name the installer uses, so running it twice is harmless.

$ErrorActionPreference = 'Continue'

$taskName = 'wudo-service'
$bin      = 'C:\wudo\bin\wudosvc.exe'
$report   = New-Object System.Text.StringBuilder

function Say([string]$text) {
    [void]$report.AppendLine($text)
    Write-Host $text
}
function Title([string]$text) {
    Say ''
    Say ('=== ' + $text + ' ===')
}
function Try-Say([scriptblock]$block) {
    try { Say (& $block) } catch { Say ('  error: ' + $_.Exception.Message) }
}

Say ('wudo task diagnostic - ' + (Get-Date).ToString('yyyy-MM-dd HH:mm:ss'))

Title '1. WHO IS RUNNING THIS'
Try-Say { 'elevated : ' + [bool]((whoami /groups) -match 'S-1-16-12288') }
Try-Say { 'identity : ' + (whoami) }
Try-Say { 'sid      : ' + ((whoami /user /fo csv /nh) -split ',')[1].Trim('"') }
Try-Say { 'windows  : ' + (Get-CimInstance Win32_OperatingSystem).Caption + ' build ' + [Environment]::OSVersion.Version.Build }

Title '2. THE DESKTOP USER (who has to log on for the task to fire)'
Try-Say {
    $shell = (Get-Process explorer -ErrorAction SilentlyContinue | Select-Object -First 1)
    if ($shell) {
        $owner = (Get-CimInstance Win32_Process -Filter ('ProcessId=' + $shell.Id) |
                  Invoke-CimMethod -MethodName GetOwner)
        'explorer : ' + $owner.Domain + '\' + $owner.User + ' (session ' + $shell.SessionId + ')'
    } else {
        'explorer : not running'
    }
}
Try-Say {
    $me = ((whoami /user /fo csv /nh) -split ',')[1].Trim('"')
    'my sid   : ' + $me
    $t = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if (-not $t) { return 'task     : not registered at all' }
    $owner = $t.Principal.UserId
    'task sid : ' + $owner
    if ($owner -and ($owner -ne $me) -and ($owner -notlike '*\' + $env:USERNAME)) {
        'MISMATCH : the task belongs to ' + $owner + ', so it will never fire for ' + $me
    } else {
        'match    : the task belongs to the same account that is logged on'
    }
}

Title '3. DOES THE TASK EXIST'
Try-Say { 'file C:\Windows\System32\Tasks\' + $taskName + ' : ' + (Test-Path ('C:\Windows\System32\Tasks\' + $taskName)) }
Try-Say { 'schtasks /Query :'; (schtasks /Query /TN $taskName /V /FO LIST 2>&1 | Out-String).Trim() }
Try-Say {
    $t = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if (-not $t) { return 'Get-ScheduledTask: NOT FOUND' }
    'Get-ScheduledTask: found'
    '  state    : ' + $t.State
    '  runLevel : ' + $t.Principal.RunLevel
    '  logonType: ' + $t.Principal.LogonType
    '  userId   : ' + $t.Principal.UserId
    '  trigger  : ' + $t.Triggers[0].CimClass.CimClassName
    '  action   : ' + $t.Actions[0].Execute
}
Try-Say {
    $all = Get-ScheduledTask -ErrorAction SilentlyContinue | Where-Object { $_.TaskName -like '*wudo*' }
    'tasks mentioning wudo: ' + (($all | ForEach-Object { $_.TaskName }) -join ', ')
}

Title '4. WHAT THE INSTALLER RECORDED'
Try-Say {
    $log = 'C:\wudo\install.log'
    if (-not (Test-Path $log)) { return 'install.log missing' }
    (Get-Content $log -Raw).Trim()
}

Title '5. THE SERVICE'
Try-Say {
    $p = Get-Process wudosvc -ErrorAction SilentlyContinue
    if (-not $p) { return 'wudosvc: not running' }
    $p | ForEach-Object { 'pid ' + $_.Id + ' session ' + $_.SessionId + ' path ' + $_.Path }
}
Try-Say {
    $ep = 'C:\wudo\endpoint.json'
    if (Test-Path $ep) { (Get-Content $ep -Raw).Trim() } else { 'endpoint.json missing' }
}

Title '6. WHY CREATION MIGHT BE BLOCKED'
Try-Say { 'schedule service: ' + ((sc.exe query Schedule | Select-String 'STATE').ToString().Trim()) }
Try-Say {
    $pol = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\Task Scheduler'
    if (Test-Path $pol) { (Get-ItemProperty $pol | Out-String).Trim() } else { 'no Task Scheduler policy' }
}
Try-Say {
    $av = Get-CimInstance -Namespace root/SecurityCenter2 -ClassName AntiVirusProduct -ErrorAction SilentlyContinue
    'antivirus: ' + (($av | ForEach-Object { $_.displayName }) -join ', ')
}
Try-Say { 'group policy processing: ' + ((gpresult /r 2>$null | Select-String 'Wudo|Task' | Select-Object -First 3) -join ' ') }

Title '7. RECENT TASK SCHEDULER ERRORS'
Try-Say {
    $ev = Get-WinEvent -LogName 'Microsoft-Windows-TaskScheduler/Operational' -MaxEvents 80 -ErrorAction SilentlyContinue |
          Where-Object { $_.LevelDisplayName -ne 'Information' } | Select-Object -First 6
    if (-not $ev) { return 'none in the last 80 events' }
    ($ev | ForEach-Object { $_.TimeCreated.ToString('yyyy-MM-dd HH:mm:ss') + ' [' + $_.Id + '] ' + ($_.Message -replace '\s+', ' ') }) -join [Environment]::NewLine
}

Title '8. REPAIR ATTEMPT (registers the task the way the installer does)'
$sid = ((whoami /user /fo csv /nh) -split ',')[1].Trim('"')
Say ('using sid: ' + $sid)
$xml = @"
<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>wudo service - starts elevated at logon so that wudo commands can run as administrator.</Description>
    <URI>\$taskName</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>$sid</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>$sid</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>$bin</Command>
    </Exec>
  </Actions>
</Task>
"@
$tmp = Join-Path $env:TEMP 'wudo-diag-task.xml'
$bytes = [byte[]](0xFF, 0xFE) + [System.Text.Encoding]::Unicode.GetBytes($xml)
[System.IO.File]::WriteAllBytes($tmp, $bytes)
Say ('task xml staged at ' + $tmp)

Say '--- attempt 1: schtasks /Create /XML (what the installer tries first) ---'
$o1 = schtasks /Create /TN $taskName /XML $tmp /F 2>&1 | Out-String
Say $o1.Trim()
Say ('exit code: ' + $LASTEXITCODE)

Say '--- attempt 2: schtasks /Create /SC ONLOGON /RL HIGHEST (the fallback) ---'
$o2 = schtasks /Create /TN $taskName /TR $bin /SC ONLOGON /RL HIGHEST /F 2>&1 | Out-String
Say $o2.Trim()
Say ('exit code: ' + $LASTEXITCODE)

Title '9. STATE AFTER THE ATTEMPT'
Try-Say { (schtasks /Query /TN $taskName /V /FO LIST 2>&1 | Out-String).Trim() }
Try-Say {
    $t = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if (-not $t) { return 'task still NOT present - see the errors above' }
    'task present: ' + $t.State + ' / ' + $t.Principal.RunLevel + ' / ' + $t.Principal.LogonType + ' / ' + $t.Actions[0].Execute
}

Title '10. DOES THE TASK ACTUALLY START THE SERVICE'
Try-Say {
    $before = @(Get-Process wudosvc -ErrorAction SilentlyContinue)
    schtasks /Run /TN $taskName 2>&1 | Out-String | ForEach-Object { $_.Trim() }
    Start-Sleep -Seconds 4
    $after = @(Get-Process wudosvc -ErrorAction SilentlyContinue)
    'processes before: ' + $before.Count + '  after: ' + $after.Count
    if ($after.Count -gt 0) { 'pids: ' + (($after | ForEach-Object { $_.Id }) -join ', ') }
}

Title 'VERDICT'
Try-Say {
    $t = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if (-not $t) {
        return 'The task could not be created. The schtasks output above contains the reason.'
    }
    $problems = @()
    if ($t.Principal.RunLevel -ne 'Highest') { $problems += 'run level is not Highest (commands would not be elevated)' }
    if ($t.Principal.LogonType -ne 'Interactive') { $problems += 'logon type is not InteractiveToken (no tray icon)' }
    if ($t.Actions[0].Execute -ne $bin) { $problems += 'action points at ' + $t.Actions[0].Execute }
    if ($t.Triggers[0].CimClass.CimClassName -ne 'MSFT_TaskLogonTrigger') { $problems += 'no logon trigger' }
    if ($problems.Count -eq 0) {
        return 'OK: the task exists, runs elevated at logon and starts ' + $bin + '. Reboot or sign out to confirm it comes up by itself.'
    }
    return 'Task exists but: ' + ($problems -join '; ')
}

$file = Join-Path $env:TEMP 'wudo-diag.txt'
try { Set-Content -Path $file -Value $report.ToString() -Encoding UTF8 } catch { }
Say ''
Say ('full report: ' + $file)
Say 'Paste that file (or the text above) to get it analysed.'
