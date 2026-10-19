# ask.ps1: run the noskill status page (Overview, Approvals, Downloads, History) from this source
# tree on Windows and open it in your browser. The Windows twin of ask.sh.
#
#   .\ask.ps1 [overview|approvals|downloads|history]     (default: approvals)
#
# Once a noskill with the `ui` command is installed you don't need this: run `noskill ui`. This is
# for a source tree: it builds the current code into a scratch folder and serves just the page and
# API with `serve --no-jobs`, so it can run next to the daemon. Running it again stops its earlier
# server and starts one from the current source.
#
# Settings (environment): NOSKILL_HOME (default %USERPROFILE%\.noskill), NOSKILL_UI_PORT (11436),
# NOSKILL_NO_OPEN (set to anything: don't open a browser). Needs Go (https://go.dev/dl/). Stop with Ctrl-C.
param([ValidateSet("overview", "approvals", "downloads", "history")][string]$Tab = "approvals")
$ErrorActionPreference = "Stop"

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$home_ = if ($env:NOSKILL_HOME) { $env:NOSKILL_HOME } else { Join-Path $env:USERPROFILE ".noskill" }
$port = if ($env:NOSKILL_UI_PORT) { [int]$env:NOSKILL_UI_PORT } else { 11436 }
$url = "http://127.0.0.1:$port/ui#$Tab"
$pidFile = Join-Path ([IO.Path]::GetTempPath()) "noskill-ask-$port.pid"

function Answering { try { Invoke-WebRequest "http://127.0.0.1:$port/" -UseBasicParsing -TimeoutSec 2 | Out-Null; $true } catch { $false } }

# Stop an earlier ask.ps1 server (found by its recorded process ID, and only if it is still a
# noskill started from a noskill-ask folder); anything else on the port is left alone.
if (Test-Path $pidFile) {
  $old = [int](Get-Content $pidFile -ErrorAction SilentlyContinue)
  $p = Get-Process -Id $old -ErrorAction SilentlyContinue
  if ($p -and $p.Path -like "*noskill-ask*") {
    Write-Host "stopping the earlier ask.ps1 server ($old) so this one runs the current source"
    Stop-Process -Id $old
    Start-Sleep -Seconds 1
  }
  Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
}
if (Answering) { Write-Error "port $port is used by something that isn't an ask.ps1 server; set `$env:NOSKILL_UI_PORT to another port"; exit 1 }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Write-Error "ask.ps1 builds from source and needs Go (https://go.dev/dl/)"; exit 1 }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("noskill-ask." + [Guid]::NewGuid().ToString("N").Substring(0, 8))
New-Item -ItemType Directory -Path $tmp | Out-Null
$proc = $null
try {
  Write-Host "building noskill from $here ..."
  Push-Location $here
  try { go build -o (Join-Path $tmp "noskill.exe") ./cmd/noskill; if ($LASTEXITCODE -ne 0) { throw "go build failed" } } finally { Pop-Location }

  $proc = Start-Process -FilePath (Join-Path $tmp "noskill.exe") -ArgumentList @("--home", "`"$home_`"", "serve", "--no-jobs", "--port", "$port") -NoNewWindow -PassThru
  Set-Content $pidFile $proc.Id
  for ($i = 0; -not (Answering); $i++) {
    if ($proc.HasExited) { Write-Error "the server stopped; see the message above"; exit 1 }
    if ($i -gt 60) { Write-Error "the server didn't come up on port $port"; exit 1 }
    Start-Sleep -Milliseconds 500
  }
  Write-Host "status page: $url   (Ctrl-C stops it)"
  if (-not $env:NOSKILL_NO_OPEN) { Start-Process $url }
  $proc.WaitForExit()
} finally {
  if ($proc -and -not $proc.HasExited) { Stop-Process -Id $proc.Id -ErrorAction SilentlyContinue }   # only the server this script started
  Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 300
  Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue                                      # our own scratch folder
}
