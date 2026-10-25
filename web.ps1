# web.ps1: the local web pages in one run (Windows; web.sh is the same for macOS and Linux).
#
#   .\web.ps1               build the docs site, then serve it and the Phase 0 spike status
#   .\web.ps1 -NoBuild      serve the docs site already in site\
#   .\web.ps1 -Open         also open the pages in your browser
#
# docs site            http://127.0.0.1:8767/   (DOCS_PORT changes it)
# Phase 0 spike status http://127.0.0.1:8765/   (SPIKES_PORT changes it)
# The noskill status page's Spikes tab shows both and expects those two ports.
# Ctrl-C stops the two servers it started, by process id, and nothing else. Needs Python 3; Go to build the site.
param([switch]$NoBuild, [switch]$Open)
$ErrorActionPreference = "Stop"
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$docsPort = if ($env:DOCS_PORT) { $env:DOCS_PORT } else { "8767" }
$spikesPort = if ($env:SPIKES_PORT) { $env:SPIKES_PORT } else { "8765" }
$py = (Get-Command python -ErrorAction SilentlyContinue), (Get-Command python3 -ErrorAction SilentlyContinue) | Where-Object { $_ } | Select-Object -First 1
if (-not $py) { [Console]::Error.WriteLine("web.ps1: Python 3 is needed to serve the pages"); exit 1 }

function Serving([string]$port) {
  try { Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "http://127.0.0.1:$port/" | Out-Null; return $true } catch { return $false }
}
function Busy([string]$port) {
  [bool](Get-NetTCPConnection -State Listen -LocalPort ([int]$port) -ErrorAction SilentlyContinue)
}

if (-not $NoBuild) {
  if (Get-Command go -ErrorAction SilentlyContinue) {
    Write-Host "building the docs site..."
    Push-Location $here
    try { go run ./scripts/docsite -out site } catch { [Console]::Error.WriteLine("web.ps1: the docs site didn't build; serving what is in site\ if anything") } finally { Pop-Location }
  } else { [Console]::Error.WriteLine("go isn't installed: serving the docs site as it is in site\") }
}
if (-not (Test-Path (Join-Path $here "site\index.html"))) { [Console]::Error.WriteLine("web.ps1: there is no docs site in site\ yet; install Go and run web.ps1 again"); exit 1 }

$procs = @()
foreach ($s in @(@{ Name = "docs site"; Port = $docsPort; Args = @("-m", "http.server", $docsPort, "--bind", "127.0.0.1", "--directory", "site") },
                 @{ Name = "spike status"; Port = $spikesPort; Args = @("spikes\status\server.py", $spikesPort) })) {
  if (Busy $s.Port) {
    if (Serving $s.Port) { Write-Host "something already serves port $($s.Port): using it for the $($s.Name)"; continue }
    [Console]::Error.WriteLine("web.ps1: port $($s.Port) is in use by something that isn't a web page"); exit 1
  }
  $procs += Start-Process -FilePath $py.Source -ArgumentList $s.Args -WorkingDirectory $here -WindowStyle Hidden -PassThru
}
Start-Sleep -Seconds 1
Write-Host ""
Write-Host "  docs site             http://127.0.0.1:$docsPort/"
Write-Host "  Phase 0 spike status  http://127.0.0.1:$spikesPort/"
Write-Host ""
Write-Host "Ctrl-C stops the servers this started."
if ($Open) { Start-Process "http://127.0.0.1:$docsPort/"; Start-Process "http://127.0.0.1:$spikesPort/" }
try { while ($true) { Start-Sleep -Seconds 1 } }
finally { foreach ($p in $procs) { if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } } }
