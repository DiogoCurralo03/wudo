# Verifies the two disconnect directions:
#   A) client dies  -> the service terminates the command it was running
#   B) service dies -> the client exits instead of hanging
param(
    [string]$Arch      = (& go env GOARCH).Trim(),
    [string]$ServiceExe = (Join-Path $PSScriptRoot "..\bin\$Arch\wudosvc-test.exe"),
    [string]$ClientExe  = (Join-Path $PSScriptRoot "..\bin\$Arch\wudo.exe"),
    [string]$StateDir   = (Join-Path $env:TEMP 'wudo-disconnect')
)

$ErrorActionPreference = 'Continue'
$service = (Get-Item $ServiceExe).FullName
$client  = (Get-Item $ClientExe).FullName
$failures = 0

function Check([string]$label, [string]$want, [string]$got) {
    if ($want -eq $got) { Write-Host ("ok   {0}" -f $label) }
    else { Write-Host ("FAIL {0}: want [{1}] got [{2}]" -f $label, $want, $got); $script:failures++ }
}

$env:WUDO_DIR = $StateDir
$env:WUDO_QUIET = '1'
$env:WUDO_AUTOACTIVATE = '10m'
if (Test-Path $StateDir) { Remove-Item $StateDir -Recurse -Force -ErrorAction SilentlyContinue }
New-Item -ItemType Directory -Path $StateDir -Force | Out-Null

$svc = Start-Process -FilePath $service -PassThru -WindowStyle Hidden
for ($i = 0; $i -lt 100; $i++) {
    if (Test-Path (Join-Path $StateDir 'endpoint.json')) { break }
    Start-Sleep -Milliseconds 100
}

try {
    Write-Host '== A) client is killed while its command runs'
    $marker = 'wudo-disconnect-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
    # ping.cmd keeps a named child process alive so its death is observable.
    $cli = Start-Process -FilePath $client -ArgumentList @('cmd', '/c', "ping -n 90 127.0.0.1 >nul & echo $marker") -PassThru -WindowStyle Hidden
    Start-Sleep -Seconds 2
    $before = (Get-Process ping -ErrorAction SilentlyContinue | Measure-Object).Count
    Check 'ping running before the kill' '1' "$before"

    Stop-Process -Id $cli.Id -Force
    Start-Sleep -Seconds 4
    $after = (Get-Process ping -ErrorAction SilentlyContinue | Measure-Object).Count
    Check 'command killed after the client died' '0' "$after"

    $log = Get-Content (Join-Path $StateDir 'wudosvc.log') -Raw
    if ($log -match 'reason=terminated') {
        Write-Host 'ok   service logged the termination'
    } else {
        Write-Host 'FAIL service did not log a termination'
        $failures++
    }

    Write-Host '== B) service is killed while the client waits'
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $cli2 = Start-Process -FilePath $client -ArgumentList @('cmd', '/c', 'ping -n 90 127.0.0.1 >nul') -PassThru -WindowStyle Hidden
    Start-Sleep -Seconds 2
    Stop-Process -Id $svc.Id -Force
    $exited = $cli2.WaitForExit(15000)
    $sw.Stop()
    Check 'client exited after the service died' 'True' "$exited"
    if ($exited) {
        Write-Host ("ok   client gave up after {0:N1}s (exit code {1})" -f $sw.Elapsed.TotalSeconds, $cli2.ExitCode)
        Check 'client exit code when the service vanished' '1' "$($cli2.ExitCode)"
    }
    $leftover = (Get-Process ping -ErrorAction SilentlyContinue | Measure-Object).Count
    Check 'no orphan ping left behind' '0' "$leftover"
}
finally {
    if ($svc -and -not $svc.HasExited) { Stop-Process -Id $svc.Id -Force -ErrorAction SilentlyContinue }
    Get-Process ping -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
}

Write-Host ''
if ($failures -eq 0) { Write-Host 'DISCONNECT BEHAVIOUR OK'; exit 0 }
Write-Host ("{0} CHECK(S) FAILED" -f $failures)
exit 1
