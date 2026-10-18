# noskill installer for Windows (docs/PACKAGING.md "Install scripts"). Per-user: no UAC prompt.
#
#   $env:NOSKILL_REPO = "owner/name"; .\install.ps1 [-NoInit] [-Version v0.1.0] [-Dir "$env:LOCALAPPDATA\Programs\noskill"]
#
# Downloads the release zip and checksums.txt over HTTPS, checks the SHA-256, installs noskill.exe,
# adds the folder to your user PATH and runs `noskill init`. It never installs Ollama.
param(
  [switch]$NoInit,
  [string]$Version = $(if ($env:NOSKILL_VERSION) { $env:NOSKILL_VERSION } else { "latest" }),
  [string]$Dir = $(if ($env:NOSKILL_INSTALL_DIR) { $env:NOSKILL_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\noskill" }),
  [string]$Repo = $env:NOSKILL_REPO
)
$ErrorActionPreference = "Stop"
if (-not $Repo) { throw "set NOSKILL_REPO=owner/name (the GitHub repository to install from)" }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) { "AMD64" { "amd64" } "ARM64" { "arm64" } default { throw "unsupported CPU: $env:PROCESSOR_ARCHITECTURE" } }
if ($Version -eq "latest") {
  $Version = (Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest").tag_name
}
$ver = $Version.TrimStart("v")
$name = "noskill_${ver}_windows_${arch}.zip"
$base = "https://github.com/$Repo/releases/download/$Version"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("noskill-" + [Guid]::NewGuid())
New-Item -ItemType Directory $tmp | Out-Null
try {
  Write-Host "downloading $name ($Version)"
  Invoke-WebRequest "$base/$name" -OutFile (Join-Path $tmp $name)
  Invoke-WebRequest "$base/checksums.txt" -OutFile (Join-Path $tmp "checksums.txt")
  $line = Get-Content (Join-Path $tmp "checksums.txt") | Where-Object { $_ -match " $([regex]::Escape($name))$" } | Select-Object -First 1
  if (-not $line) { throw "$name isn't listed in checksums.txt" }
  $want = ($line -split "\s+")[0]
  $got = (Get-FileHash (Join-Path $tmp $name) -Algorithm SHA256).Hash.ToLower()
  if ($got -ne $want) { throw "checksum mismatch: the download is not what the release lists. Nothing was installed." }
  Write-Host "checksum ok"
  Write-Host "to check the signature yourself: gh attestation verify <file> --repo $Repo"

  Expand-Archive (Join-Path $tmp $name) -DestinationPath $tmp -Force
  New-Item -ItemType Directory $Dir -Force | Out-Null
  $target = Join-Path $Dir "noskill.exe"
  if (Test-Path $target) { Move-Item $target "$target.old" -Force }   # a running exe can be renamed, not overwritten
  Move-Item (Join-Path $tmp "noskill.exe") $target
  Write-Host "installed $target"

  $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
  if (($userPath -split ";") -notcontains $Dir) {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$Dir", "User")
    Write-Host "added $Dir to your user PATH; open a new terminal to use it"
  }
  if (-not $NoInit) { & $target init }
} finally {
  Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
