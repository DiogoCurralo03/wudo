# Smoke test for the wudo binaries.
#
# wudosvc.exe carries a manifest that demands administrator rights, so it cannot
# be started from a non elevated shell. This script therefore builds a temporary
# copy without that manifest, which lets the whole pipeline be exercised without
# UAC. Everything else - the tray-free service core, the real client, the wire
# protocol, ConPTY, job objects - is exactly the production code.
#
# Usage:  powershell -ExecutionPolicy Bypass -File scripts\smoke.ps1

param(
    [string]$StateDir = (Join-Path $env:TEMP 'wudo-smoke')
)

$ErrorActionPreference = 'Continue'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$arch    = (& go env GOARCH).Trim()
$service = Join-Path $root "bin\$arch\wudosvc-test.exe"
$client  = Join-Path $root "bin\$arch\wudo.exe"

$failures = 0
function Check([string]$label, [string]$want, [string]$got) {
    if ($want -eq $got) {
        Write-Host ("ok   {0}" -f $label)
    } else {
        Write-Host ("FAIL {0}: want [{1}] got [{2}]" -f $label, $want, $got)
        $script:failures++
    }
}
function CheckContains([string]$label, [string]$needle, [string]$haystack) {
    if ($haystack -like "*$needle*") {
        Write-Host ("ok   {0}" -f $label)
    } else {
        Write-Host ("FAIL {0}: [{1}] does not contain [{2}]" -f $label, $haystack, $needle)
        $script:failures++
    }
}

# --- build a manifest-less service and the client if needed -------------------
# The real wudosvc.exe demands administrator rights through its manifest, so the
# smoke test uses a debug copy that can run from an ordinary shell. Everything
# else - runner, ConPTY, job objects, protocol - is the production code.
Push-Location $root
try {
    if (-not (Test-Path $client)) {
        Write-Host 'building wudo.exe...'
        & go build -o $client './cmd/wudo'
        if ($LASTEXITCODE -ne 0) { throw 'build wudo.exe failed' }
    }
    if (-not (Test-Path $service)) {
        Write-Host 'building wudosvc-test.exe (no elevation manifest)...'
        $debugDir = Join-Path $env:TEMP ('wudo-debug-' + [guid]::NewGuid().ToString('N'))
        & (Join-Path $PSScriptRoot 'build.ps1') -Arch $arch -NoElevationManifest -OutDir $debugDir | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'build wudosvc-test.exe failed' }
        Copy-Item (Join-Path $debugDir 'wudosvc.exe') $service -Force
        Remove-Item $debugDir -Recurse -Force -ErrorAction SilentlyContinue
        # The debug build rewrote the production .syso of this architecture;
        # regenerate it so the repository keeps the elevation-enabled resources.
        Write-Host 'restoring the production resources...'
        & (Join-Path $PSScriptRoot 'build.ps1') -Arch $arch | Out-Null
    }
}
finally {
    Pop-Location
}

function Start-WudoService([string]$autoActivate) {
    $env:WUDO_DIR = $StateDir
    $env:WUDO_QUIET = '1'
    if ($autoActivate) { $env:WUDO_AUTOACTIVATE = $autoActivate }
    else { Remove-Item Env:\WUDO_AUTOACTIVATE -ErrorAction SilentlyContinue }
    if (Test-Path $StateDir) { Remove-Item $StateDir -Recurse -Force -ErrorAction SilentlyContinue }
    New-Item -ItemType Directory -Path $StateDir -Force | Out-Null
    $p = Start-Process -FilePath $service -PassThru -WindowStyle Hidden
    for ($i = 0; $i -lt 100; $i++) {
        if (Test-Path (Join-Path $StateDir 'endpoint.json')) { break }
        Start-Sleep -Milliseconds 100
    }
    return $p
}

function Stop-WudoService($proc) {
    if ($proc -and -not $proc.HasExited) {
        Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        $proc.WaitForExit(5000) | Out-Null
    }
}

# --- 1) service running but inactive -----------------------------------------
$proc = Start-WudoService $null
try {
    Write-Host '== service inactive'
    $out = & $client cmd /c "echo should-not-run" 2>&1 | Out-String
    Check 'inactive exit code' '1' "$LASTEXITCODE"
    CheckContains 'inactive message' 'service not active' $out

    & $client --status | Out-Null
    Check 'status exit code for inactive service' '0' "$LASTEXITCODE"
}
finally {
    Stop-WudoService $proc
}

# --- 2) active service --------------------------------------------------------
$proc = Start-WudoService '5m'
try {
    Write-Host '== service active'
    & $client --status

    $out = (& $client whoami | Out-String).Trim()
    Check 'whoami exit code' '0' "$LASTEXITCODE"
    CheckContains 'whoami output' $env:USERNAME $out.ToLower()

    & $client cmd /c "exit /b 33" | Out-Null
    Check 'exit code propagation' '33' "$LASTEXITCODE"

    $stdout = (& $client --shell "echo to-stdout & echo to-stderr 1>&2" 2>$null | Out-String).Trim()
    Check 'stdout separated from stderr' 'to-stdout' $stdout

    $tty = (& $client -t cmd /d /s /c "echo tty-ran" | Out-String) -replace "`r", ''
    CheckContains 'tty mode output' 'tty-ran' $tty

    $stdin = ('line-one' + [Environment]::NewLine + 'line-two' + [Environment]::NewLine) | & $client sort | Out-String
    CheckContains 'stdin forwarded' 'line-one' $stdin

    $cwd = (& $client cmd /c cd | Out-String).Trim()
    Check 'client working directory used' (Get-Location).Path $cwd

    if (Get-Command npm.cmd -ErrorAction SilentlyContinue) {
        $npm = (& $client npm.cmd --version 2>&1 | Out-String).Trim()
        CheckContains 'npm.cmd shim runs' '.' $npm
    } else {
        Write-Host 'skip npm.cmd shim (npm is not installed)'
    }

    Write-Host '== concurrent commands'
    $jobs = @()
    for ($i = 1; $i -le 5; $i++) {
        $jobs += Start-Job -ScriptBlock {
            param($c, $i, $dir)
            $env:WUDO_DIR = $dir
            $env:WUDO_TTY = '0'
            (& $c cmd /c "ping -n 2 127.0.0.1 >nul & echo concurrent-$i" | Out-String)
        } -ArgumentList $client, $i, $StateDir
    }
    $jobs | Wait-Job -Timeout 120 | Out-Null
    $ok = 0
    foreach ($j in $jobs) {
        $r = Receive-Job $j
        if ($r -match 'concurrent-') { $ok++ }
        Remove-Job $j -Force
    }
    Check 'concurrent commands' '5' "$ok"

    Write-Host '== long command while another runs'
    $long = Start-Job -ScriptBlock {
        param($c, $dir)
        $env:WUDO_DIR = $dir
        $env:WUDO_TTY = '0'
        (& $c cmd /c "ping -n 4 127.0.0.1 >nul & echo long-done" | Out-String)
    } -ArgumentList $client, $StateDir
    Start-Sleep -Seconds 1
    & $client cmd /c "echo quick" | Out-Null
    Check 'short command while long one runs' '0' "$LASTEXITCODE"
    $longOut = Receive-Job ($long | Wait-Job -Timeout 60)
    Remove-Job $long -Force
    CheckContains 'long command finished' 'long-done' $longOut

    Write-Host '== timeout'
    $sw = [Diagnostics.Stopwatch]::StartNew()
    & $client --timeout 3s cmd /c "ping -n 30 127.0.0.1 >nul" | Out-Null
    $sw.Stop()
    Check 'timeout exit code' '124' "$LASTEXITCODE"
    if ($sw.Elapsed.TotalSeconds -lt 15) {
        Write-Host ("ok   timeout enforced ({0:N1}s)" -f $sw.Elapsed.TotalSeconds)
    } else {
        Write-Host ("FAIL timeout took {0:N1}s" -f $sw.Elapsed.TotalSeconds)
        $failures++
    }

    Write-Host '== service log (tail)'
    Get-Content (Join-Path $StateDir 'wudosvc.log') -Tail 4
}
finally {
    Stop-WudoService $proc
}

Write-Host ''
if ($failures -eq 0) {
    Write-Host 'ALL SMOKE CHECKS PASSED'
    exit 0
}
Write-Host ("{0} CHECK(S) FAILED" -f $failures)
exit 1
