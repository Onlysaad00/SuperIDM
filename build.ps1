<#
.SYNOPSIS
  Builds SuperIDM on Windows: the desktop .exe, the CLI .exe and the browser
  extension zip.

.EXAMPLE
  .\build.ps1
  .\build.ps1 -Version 1.2.0 -Zip
  .\build.ps1 -Test

.NOTES
  Requirements: Go 1.22+ (winget install GoLang.Go) and PowerShell 5.1+.
  No CGO, no MSVC, no resource compiler and no installer framework needed.
#>

[CmdletBinding()]
param(
  [string] $Version = "",
  [string] $Out = "dist",
  [switch] $Zip,
  [switch] $Test,
  [switch] $Clean,
  [switch] $SkipArm64
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Definition
Set-Location $root

function Say($msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Warn($msg) { Write-Host "!!  $msg" -ForegroundColor Yellow }

if ($Clean) {
  Say "Cleaning"
  Remove-Item -Recurse -Force $Out -ErrorAction SilentlyContinue
  if (-not $Zip -and -not $Test) { return }
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  throw "Go is not installed or not on PATH. Install it with:  winget install GoLang.Go"
}

if (-not $Version) {
  $Version = (git describe --tags --always --dirty 2>$null)
  if (-not $Version) { $Version = "1.0.0" }
}
$Version = $Version.TrimStart("v")
Say "Version  : $Version"
Say "Go       : $(go version)"
Say "Output   : $Out"

New-Item -ItemType Directory -Force -Path $Out | Out-Null

$ldflags = "-s -w -X main.Version=$Version"

# ---------------------------------------------------------------- Windows x64
Say "Building $Out\SuperIDM.exe (GUI, x64)"
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags "$ldflags -H=windowsgui" -o "$Out\SuperIDM.exe" .\cmd\superidm
if ($LASTEXITCODE -ne 0) { throw "build failed" }

Say "Building $Out\SuperIDM-cli.exe (console, x64)"
go build -trimpath -ldflags $ldflags -o "$Out\SuperIDM-cli.exe" .\cmd\superidm
if ($LASTEXITCODE -ne 0) { throw "build failed" }

if (-not $SkipArm64) {
  Say "Building $Out\SuperIDM-arm64.exe (GUI, arm64)"
  $env:GOARCH = "arm64"
  go build -trimpath -ldflags "$ldflags -H=windowsgui" -o "$Out\SuperIDM-arm64.exe" .\cmd\superidm
  $env:GOARCH = "amd64"
}

# ----------------------------------------------------------------------- tests
if ($Test) {
  Say "Running tests"
  go test ./... -timeout 300s
  if ($LASTEXITCODE -ne 0) { throw "tests failed" }
}

# ------------------------------------------------------------------- resources
if (-not (Test-Path "chrome-extension\icons\icon128.png") -or -not (Test-Path "assets\superidm.ico")) {
  Say "Generating icon assets"
  if (Get-Command python -ErrorAction SilentlyContinue) {
    python tools\make_icons.py
  } else {
    Warn "Python not found: icons are missing and could not be regenerated."
  }
}

# --------------------------------------------------------------- extension zip
$extZip = "$Out\SuperIDM-chrome-extension-$Version.zip"
if (Test-Path $extZip) { Remove-Item $extZip }
Say "Packaging extension -> $extZip"
Compress-Archive -Path "chrome-extension\*" -DestinationPath $extZip -Force

# ----------------------------------------------------------------- bundle + sums
if ($Zip) {
  $stage = Join-Path $env:TEMP "superidm-stage-$([guid]::NewGuid().ToString('N'))"
  $bundleDir = Join-Path $stage "SuperIDM"
  New-Item -ItemType Directory -Force -Path $bundleDir | Out-Null
  Copy-Item "$Out\SuperIDM.exe","$Out\SuperIDM-cli.exe" $bundleDir -ErrorAction SilentlyContinue
  Copy-Item "$Out\SuperIDM-arm64.exe" $bundleDir -ErrorAction SilentlyContinue
  Copy-Item "chrome-extension" $bundleDir -Recurse
  Copy-Item "README.md","LICENSE" $bundleDir -ErrorAction SilentlyContinue
  Copy-Item "docs" $bundleDir -Recurse -ErrorAction SilentlyContinue

  @"
SuperIDM
========

1. Run SuperIDM.exe                 (the desktop app opens)
2. Open the "Extension & setup" tab inside the app
3. In Chrome/Edge:  chrome://extensions  ->  enable "Developer mode"
   ->  "Load unpacked"  ->  select the chrome-extension folder next to this file
4. That's it. Downloads, videos and streams will now go through SuperIDM.

Command line:
   SuperIDM-cli.exe -d "https://example.com/big.iso" -n 64
   SuperIDM-cli.exe --inspect "https://example.com/big.iso"
"@ | Set-Content (Join-Path $bundleDir "START-HERE.txt")

  $bundle = "$Out\SuperIDM-v$Version-windows-x64.zip"
  if (Test-Path $bundle) { Remove-Item $bundle }
  Say "Building release bundle -> $bundle"
  Compress-Archive -Path (Join-Path $stage "SuperIDM") -DestinationPath $bundle -Force
  Remove-Item -Recurse -Force $stage
}

Say "Writing SHA256SUMS.txt"
Get-ChildItem $Out -File | Where-Object { $_.Name -ne "SHA256SUMS.txt" } | ForEach-Object {
  $h = (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower()
  "{0}  {1}" -f $h, $_.Name
} | Set-Content "$Out\SHA256SUMS.txt"

Say "Done. Artifacts:"
Get-ChildItem $Out -File | Format-Table Name, @{N="Size (MB)";E={[math]::Round($_.Length/1MB,2)}} -AutoSize
