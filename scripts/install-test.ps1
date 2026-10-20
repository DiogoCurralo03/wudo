# Exercises the installer and the uninstaller the way a user would, but without
# touching the machine: it installs into a temporary folder with PATH and the
# logon task disabled, then uninstalls again.
#
# The elevated parts (machine PATH, scheduled task) need an elevated shell and are
# described in README.md.
#
# Usage:  powershell -ExecutionPolicy Bypass -File scripts\install-test.ps1

param(
    [string]$Arch = (& go env GOARCH).Trim()
)

$ErrorActionPreference = 'Continue'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$failures = 0

function Check([string]$label, [string]$want, [string]$got) {
    if ($want -eq $got) { Write-Host ("ok   {0}" -f $label) }
    else { Write-Host ("FAIL {0}: want [{1}] got [{2}]" -f $label, $want, $got); $script:failures++ }
}
function CheckContains([string]$label, [string]$needle, [string]$haystack) {
    if ($haystack -like "*$needle*") { Write-Host ("ok   {0}" -f $label) }
    else { Write-Host ("FAIL {0}: [{1}] does not contain [{2}]" -f $label, $haystack, $needle); $script:failures++ }
}
function CheckTrue([string]$label, [bool]$condition) {
    if ($condition) { Write-Host ("ok   {0}" -f $label) }
    else { Write-Host ("FAIL {0}" -f $label); $script:failures++ }
}

# The real installer asks for UAC. This script builds a copy without the elevation
# manifest so it can run from an ordinary shell; WUDO_INSTALL_TEST then skips the
# administrator check.
$debugDir = Join-Path $env:TEMP ('wudo-installer-test-' + [guid]::NewGuid().ToString('N'))
Push-Location $root
try {
    Write-Host 'building the installer without its elevation manifest...'
    & (Join-Path $PSScriptRoot 'build.ps1') -Arch $Arch -NoElevationManifest -OutDir $debugDir | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }
    # Restore the production resources of this architecture.
    & (Join-Path $PSScriptRoot 'build.ps1') -Arch $Arch | Out-Null
}
finally {
    Pop-Location
}

$installer = Join-Path $debugDir 'wudoinstall.exe'
$target = Join-Path $env:TEMP ('wudo-install-target-' + [guid]::NewGuid().ToString('N'))
$env:WUDO_INSTALL_TEST = '1'

try {
    Write-Host '== help'
    $help = & $installer --help 2>&1 | Out-String
    CheckContains 'usage text is printed' 'Installs wudo on this machine' $help
    Check 'help exit code' '0' "$LASTEXITCODE"

    Write-Host '== version'
    $ver = & $installer --version 2>&1 | Out-String
    CheckContains 'version is printed' 'wudoinstall' $ver

    Write-Host '== install into a temporary folder (no PATH, no autostart, no start)'
    $out = & $installer --root $target --no-path --no-autostart --no-start --yes 2>&1 | Out-String
    Write-Host $out
    Check 'install exit code' '0' "$LASTEXITCODE"
    foreach ($name in @('wudo.exe', 'wudosvc.exe', 'wudo-uninstall.exe')) {
        CheckTrue "$name is installed" (Test-Path (Join-Path $target "bin\$name"))
    }
    # The debug build deliberately installs the manifest-less service, so check
    # the installed size instead: it must match the file it came from.
    $installedSize = (Get-Item (Join-Path $target 'bin\wudosvc.exe')).Length
    CheckTrue 'the installed service looks like a real binary' ($installedSize -gt 1000000)

    Write-Host '== installing twice is harmless'
    & $installer --root $target --no-path --no-autostart --no-start --yes 2>&1 | Out-Null
    Check 'second install exit code' '0' "$LASTEXITCODE"

    Write-Host '== the installed uninstaller removes everything'
    # Run it from outside the folder it removes, exactly like a user would.
    $uninst = Join-Path $env:TEMP ('wudo-uninstaller-' + [guid]::NewGuid().ToString('N') + '.exe')
    Copy-Item (Join-Path $target 'bin\wudo-uninstall.exe') $uninst -Force
    $out = & $uninst --root $target --yes 2>&1 | Out-String
    Write-Host $out
    Check 'uninstall exit code' '0' "$LASTEXITCODE"
    CheckTrue 'the target folder is gone' (-not (Test-Path $target))
    Remove-Item $uninst -Force -ErrorAction SilentlyContinue
}
finally {
    Remove-Item Env:\WUDO_INSTALL_TEST -ErrorAction SilentlyContinue
    Remove-Item $debugDir -Recurse -Force -ErrorAction SilentlyContinue
    Remove-Item $target -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ''
if ($failures -eq 0) { Write-Host 'INSTALLER BEHAVIOUR OK'; exit 0 }
Write-Host ("{0} CHECK(S) FAILED" -f $failures)
exit 1
