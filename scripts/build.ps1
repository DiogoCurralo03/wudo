# wudo - build script
#
# Builds all three binaries for one architecture:
#   * wudo.exe           - unprivileged client
#   * wudosvc.exe        - elevated tray service
#   * wudoinstall.exe    - installer (embeds both, installs as wudo-uninstall.exe too)
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File scripts\build.ps1 [-Arch arm64|amd64] [-OutDir bin] [-NoElevationManifest]
#
# -NoElevationManifest builds the service without its requireAdministrator
# manifest, which makes it startable from a non elevated shell. It is only meant
# for debugging and for scripts\smoke.ps1.

param(
    [string]$Arch   = $env:GOARCH,
    [string]$OutDir = '',
    [switch]$NoElevationManifest
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Push-Location $root
try {
    $env:GOOS = 'windows'
    if (-not $Arch) { $Arch = (& go env GOARCH).Trim() }
    $env:GOARCH = $Arch
    $env:CGO_ENABLED = '0'

    # The output folder follows the architecture unless one is given. It must not
    # come from $env:GOARCH: this script sets that variable, so a second call in
    # the same session would otherwise inherit the previous architecture.
    if (-not $OutDir) { $OutDir = $Arch }
    # A relative folder is placed under bin\, an absolute one is used as is.
    if ([System.IO.Path]::IsPathRooted($OutDir)) {
        $OutDir = [System.IO.Path]::GetFullPath($OutDir)
    } else {
        $OutDir = [System.IO.Path]::GetFullPath((Join-Path $root (Join-Path 'bin' $OutDir)))
    }
    New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

    # rsrc turns the .ico and the .exe.manifest next to each main package into a
    # .syso that the Go linker picks up automatically.
    $rsrc = Join-Path (& go env GOPATH).Trim() 'bin\rsrc.exe'
    if (-not (Test-Path $rsrc)) {
        Write-Host 'installing rsrc (resource compiler)...'
        & go install github.com/akavel/rsrc@latest
        if ($LASTEXITCODE -ne 0) { throw 'cannot install rsrc' }
    }

    $syso = "rsrc_windows_$Arch.syso"

    # The service manifest is what makes Windows ask for UAC consent. Debug
    # builds drop it so the service can run from an ordinary shell.
    $serviceManifest = 'cmd\wudosvc\wudosvc.exe.manifest'
    if ($NoElevationManifest) {
        $serviceManifest = Join-Path $env:TEMP 'wudosvc.noelevation.manifest'
        (Get-Content 'cmd\wudosvc\wudosvc.exe.manifest' -Raw) `
            -replace 'level="requireAdministrator"', 'level="asInvoker"' |
            Set-Content -Path $serviceManifest -Encoding UTF8
    }

    # Same idea for the installer: without its manifest it can be started from an
    # ordinary shell, which the test suite uses to exercise the guard rails.
    $installerManifest = 'cmd\wudoinstall\wudoinstall.exe.manifest'
    if ($NoElevationManifest) {
        $installerManifest = Join-Path $env:TEMP 'wudoinstall.noelevation.manifest'
        (Get-Content 'cmd\wudoinstall\wudoinstall.exe.manifest' -Raw) `
            -replace 'level="requireAdministrator"', 'level="asInvoker"' |
            Set-Content -Path $installerManifest -Encoding UTF8
    }

    $targets = @(
        @{ Pkg = 'cmd\wudo';        Ico = 'cmd\wudo\wudo.ico';               Manifest = 'cmd\wudo\wudo.exe.manifest';       Exe = 'wudo.exe' },
        @{ Pkg = 'cmd\wudosvc';     Ico = 'cmd\wudosvc\wudosvc.ico';         Manifest = $serviceManifest;                   Exe = 'wudosvc.exe' },
        @{ Pkg = 'cmd\wudoinstall'; Ico = 'cmd\wudoinstall\wudoinstall.ico'; Manifest = $installerManifest;                 Exe = 'wudoinstall.exe' }
    )
    foreach ($t in $targets) {
        & $rsrc -arch $Arch -ico $t.Ico -manifest $t.Manifest -o (Join-Path $t.Pkg $syso)
        if ($LASTEXITCODE -ne 0) { throw ("rsrc failed for {0}" -f $t.Exe) }
    }

    $version = (& git describe --tags --abbrev=0 2>$null | Out-String).Trim()
    if ($version) { $version = $version.TrimStart('v') }
    if (-not $version) {
        $version = (Select-String -Path 'internal\version\version.go' -Pattern 'Version = "([^"]+)"').Matches[0].Groups[1].Value
    }
    $commit = 'dev'
    if (Test-Path (Join-Path $root '.git')) {
        $commit = (& git rev-parse --short HEAD 2>$null | Out-String).Trim()
        if (-not $commit) { $commit = 'dev' }
    }
    $date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    $ldflags = "-s -w -X 'github.com/italoalmeida0/wudo/internal/version.Version=$version' -X 'github.com/italoalmeida0/wudo/internal/version.Commit=$commit' -X 'github.com/italoalmeida0/wudo/internal/version.BuildDate=$date'"

    Write-Host "building for windows/$Arch into $OutDir"

    # 1) the two programs the installer carries
    & go build -trimpath -ldflags $ldflags -o (Join-Path $OutDir 'wudo.exe') './cmd/wudo'
    if ($LASTEXITCODE -ne 0) { throw 'build wudo.exe failed' }
    # -H windowsgui: the service is a tray application, so it must not own a
    # console window. This is what makes it start invisibly at logon.
    & go build -trimpath -ldflags "$ldflags -H windowsgui" -o (Join-Path $OutDir 'wudosvc.exe') './cmd/wudosvc'
    if ($LASTEXITCODE -ne 0) { throw 'build wudosvc.exe failed' }

    # 2) stage them as the installer payload, then build the installer
    $payload = 'cmd\wudoinstall\payload'
    New-Item -ItemType Directory -Force -Path $payload | Out-Null
    Copy-Item (Join-Path $OutDir 'wudo.exe') $payload -Force
    Copy-Item (Join-Path $OutDir 'wudosvc.exe') $payload -Force
    & go build -tags wudo_installer -trimpath -ldflags $ldflags -o (Join-Path $OutDir 'wudoinstall.exe') './cmd/wudoinstall'
    if ($LASTEXITCODE -ne 0) { throw 'build wudoinstall.exe failed' }

    # The payload is a build artefact; leaving it behind would make every later
    # build of another architecture embed the wrong binaries.
    Remove-Item $payload -Recurse -Force -ErrorAction SilentlyContinue

    Get-ChildItem $OutDir -Filter '*.exe' | Sort-Object Name | ForEach-Object {
        Write-Host ("  {0,-18} {1,12:N0} bytes" -f $_.Name, $_.Length)
    }
}
finally {
    Pop-Location
}
