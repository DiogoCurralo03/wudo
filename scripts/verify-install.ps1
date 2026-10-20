# Checks a real installation of wudo on this machine: the files, the machine PATH,
# the logon task, and the elevation the commands really get.
#
# Usage:  powershell -ExecutionPolicy Bypass -File scripts\verify-install.ps1
param([string]$Root = 'C:\wudo')

$ErrorActionPreference = 'Continue'
$failures = 0
function Check([string]$label, [string]$want, [string]$got) {
    if ($want -eq $got) { Write-Host ("ok   {0}" -f $label) }
    else { Write-Host ("FAIL {0}: want [{1}] got [{2}]" -f $label, $want, $got); $script:failures++ }
}
function CheckTrue([string]$label, [bool]$condition) {
    if ($condition) { Write-Host ("ok   {0}" -f $label) }
    else { Write-Host ("FAIL {0}" -f $label); $script:failures++ }
}
function CheckContains([string]$label, [string]$needle, [string]$haystack) {
    if ($haystack -like "*$needle*") { Write-Host ("ok   {0}" -f $label) }
    else { Write-Host ("FAIL {0}: [{1}] does not contain [{2}]" -f $label, $haystack, $needle); $script:failures++ }
}

$binDir = Join-Path $Root 'bin'
$client = Join-Path $binDir 'wudo.exe'

Write-Host '== files'
foreach ($name in @('wudo.exe', 'wudosvc.exe', 'wudo-uninstall.exe')) {
    CheckTrue "$name is installed" (Test-Path (Join-Path $binDir $name))
}
CheckTrue 'endpoint.json exists (the service published it)' (Test-Path (Join-Path $Root 'endpoint.json'))
CheckTrue 'install.log exists' (Test-Path (Join-Path $Root 'install.log'))

Write-Host '== machine PATH'
$machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
CheckTrue 'the bin folder is in the machine PATH' ($machinePath -like "*$binDir*")
CheckTrue 'it appears exactly once' ((($machinePath -split ';') | Where-Object { $_ -eq $binDir }).Count -eq 1)

Write-Host '== logon task'
$task = Get-ScheduledTask -TaskName 'wudo-service' -ErrorAction SilentlyContinue
CheckTrue 'the task exists' ($null -ne $task)
if ($task) {
    Check 'it has a logon trigger' 'MSFT_TaskLogonTrigger' "$($task.Triggers[0].CimClass.CimClassName)"
    Check 'it runs with the highest privileges' 'Highest' "$($task.Principal.RunLevel)"
    Check 'it uses an interactive token (so the tray icon shows)' 'Interactive' "$($task.Principal.LogonType)"
    Check 'it started the installed binary' (Join-Path $binDir 'wudosvc.exe') "$($task.Actions[0].Execute)"
}

Write-Host '== service state'
$status = & $client --status 2>&1 | Out-String
Write-Host $status
Check 'wudo --status exit code' '0' "$LASTEXITCODE"
CheckContains 'the service reports itself as elevated' '(elevated)' $status

Write-Host '== commands really run elevated'
$priv = & $client cmd /c "net session >nul 2>&1 && echo ELEVATED || echo NOT-ELEVATED" 2>&1 | Out-String
CheckContains 'net session succeeds through wudo' 'ELEVATED' $priv
$local = cmd /c "net session >nul 2>&1 && echo ELEVATED || echo NOT-ELEVATED"
CheckContains 'and fails locally (proving the difference)' 'NOT-ELEVATED' $local

$integrity = & $client whoami /groups 2>&1 | Out-String
CheckContains 'the command runs at high integrity' 'S-1-16-12288' $integrity

Write-Host ''
if ($failures -eq 0) { Write-Host 'INSTALLATION VERIFIED'; exit 0 }
Write-Host ("{0} CHECK(S) FAILED" -f $failures)
exit 1
