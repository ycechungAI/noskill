# start.ps1: one place to run noskill's everyday commands from a menu (Windows twin of start.sh).
#
#   .\start.cmd                  the menu (type a number or a name; q quits)
#   .\start.cmd <name> [words]   run one item straight away, e.g.  .\start.cmd research "heat pumps"
#   .\start.cmd -l | --list      list every item, one per line
#   .\start.cmd -h | --help      this text
#   .\start.cmd readme           the guide (docs\START.md)
#
# Options (before the name):  --source  use a noskill built from this source tree, even if one is installed
#                             -n | --dry  print the command instead of running it
# Settings (environment):     NOSKILL_BIN  the noskill to use   NOSKILL_HOME  the noskill folder (default %USERPROFILE%\.noskill)
#
# It uses the installed noskill. When that one doesn't know a command yet (it was installed before the
# command existed), it builds the current source into a scratch folder instead and uses that; it never
# replaces the installed one. Items starting with @ are for working on noskill itself and need Go.
# start.cmd runs this file with the execution policy relaxed for this one run, so you needn't change it.
$ErrorActionPreference = "Stop"
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$script:fromSource = $false
$script:dry = $false

# key|group|title|what it runs ({} = the words you type; @ = a source-tree task)|what to ask for|default
$table = @'
ui|Use|Status page: approvals, downloads, history|ui||
approvals|Use|What is waiting for your yes|approvals||
run|Use|Run one task|run {}|What should noskill do?|
chat|Use|Chat in the terminal|chat||
research|Use|STORM research report on a topic|run --skill storm {}|Topic to research|
ask-docs|Use|Ask your own documents|run --skill local-docs {}|Your question|
docs-add|Use|Let noskill read a folder|docs add {}|Folder to add|
anime|Anime|Tracker status|anime status||
anime-dry|Anime|Try a run without downloading|anime run --dry-run||
anime-add|Anime|Add a download by link|anime add {}|Magnet, .torrent or Nyaa link|
anime-audit|Anime|Check the last 24 hours|anime audit --since 24h||
doctor|Check|Check the setup|doctor||
models|Check|Models and their speed|models||
jobs|Check|Running jobs and recent runs|jobs||
schedule|Check|Scheduled jobs|schedule list||
skills|Check|Installed skills|skills list||
bench|Test|Benchmark a model: speed, memory, quality|models bench {}|Model to benchmark|granite4:3b
bench-speed|Test|Benchmark speed and memory only|models bench {} --perf-only|Model to benchmark|granite4:3b
eval|Test|Run the checked tasks on a model|eval run --model {}|Model to test|granite4:3b
optimise|Test|Quick trial of a better wording for a skill|skills optimise {} --model granite4:3b --dry-run|Skill to try|files
test|Dev|Go tests|@go test ./...||
vet|Dev|Vet all six targets|@vet||
site|Dev|Build the docs site|@go run ./scripts/docsite||
readme|Help|Show the guide|@readme||
'@
$items = $table -split "`r?`n" | Where-Object { $_ } | ForEach-Object {
  $f = $_ -split "\|", 6
  [pscustomobject]@{ Key = $f[0]; Group = $f[1]; Title = $f[2]; Tpl = $f[3]; Ask = $f[4]; Def = $f[5] }
}

function Usage {
  $lines = Get-Content $PSCommandPath
  for ($i = 1; $i -lt $lines.Count -and $lines[$i].StartsWith("#"); $i++) { $lines[$i] -replace "^# ?", "" }
}
function List { $items | ForEach-Object { "{0,-12} {1,-6} {2}" -f $_.Key, $_.Group, $_.Title } }

function Installed {
  if ($env:NOSKILL_BIN) { return $env:NOSKILL_BIN }
  $c = Get-Command noskill -ErrorAction SilentlyContinue
  if ($c) { return $c.Source }
  $p = Join-Path $env:LOCALAPPDATA "Programs\noskill\noskill.exe"
  if (Test-Path $p) { return $p }
}
function SourceBin {
  if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw "this needs Go to build noskill from source (https://go.dev/dl/), or install noskill" }
  $dir = Join-Path ([IO.Path]::GetTempPath()) "noskill-start"
  New-Item -ItemType Directory -Path $dir -Force | Out-Null
  $out = Join-Path $dir "noskill.exe"
  $stale = -not (Test-Path $out)
  if (-not $stale) {
    $t = (Get-Item $out).LastWriteTime
    $stale = [bool](Get-ChildItem (Join-Path $here "cmd"), (Join-Path $here "internal"), (Join-Path $here "skills") -Recurse -File -Include *.go, *.md, *.yaml, *.html | Where-Object { $_.LastWriteTime -gt $t } | Select-Object -First 1)
  }
  if ($stale) {
    [Console]::Error.WriteLine("building noskill from $here ...")
    Push-Location $here
    try { go build -o $out ./cmd/noskill; if ($LASTEXITCODE -ne 0) { throw "go build failed" } } finally { Pop-Location }
  }
  $out
}
function Supported($bin, [string[]]$words) {
  if (-not ((& $bin help 2>&1) -match "^  $([regex]::Escape($words[0])) ")) { return $false }
  for ($i = 0; $i -lt $words.Count - 1; $i++) {
    if ($words[$i] -eq "--skill") { if (-not ((& $bin skills list 2>&1) -match "^$([regex]::Escape($words[$i + 1]))\b")) { return $false } }
  }
  $true
}
function PickBin([string[]]$words) {
  if ($script:fromSource) { if ($script:dry) { return "<noskill built from this source>" }; return SourceBin }
  $b = Installed
  if ($b -and (Supported $b $words)) { return $b }
  if ($b) { [Console]::Error.WriteLine("(the installed noskill doesn't have this yet: using the current source)") }
  if ($script:dry) { return "<noskill built from this source>" }
  SourceBin
}

function RunItem([string]$key, [string[]]$more) {
  $it = $items | Where-Object { $_.Key -eq $key }
  if (-not $it) { [Console]::Error.WriteLine("no item called '$key'. .\start.cmd --list shows them."); return 2 }
  $tpl = $it.Tpl
  switch -Regex ($tpl) {
    "^@readme$" { Get-Content (Join-Path $here "docs\START.md"); return 0 }
    "^@vet$" {
      foreach ($t in "darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64") {
        $os, $arch = $t -split "/"
        Push-Location $here
        try { $env:GOOS = $os; $env:GOARCH = $arch; Write-Host -NoNewline "${t}: "; go vet ./...; if ($LASTEXITCODE -ne 0) { return 1 }; "ok" }
        finally { Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue; Pop-Location }
      }
      return 0
    }
    "^@" {
      $w = $tpl.Substring(1) -split " "
      if ($script:dry) { "(in $here) $($w -join ' ')"; return 0 }
      Push-Location $here
      try { & $w[0] @($w[1..($w.Count - 1)] + $more); return $LASTEXITCODE } finally { Pop-Location }
    }
  }
  $words = $tpl -split " "
  $answer = ""
  if ($tpl.Contains("{}")) {
    if ($more.Count -gt 0) { $answer = $more -join " " }
    elseif ($it.Ask -and [Environment]::UserInteractive -and -not [Console]::IsInputRedirected) {
      $answer = Read-Host ($it.Ask + $(if ($it.Def) { " [$($it.Def)]" } else { "" }))
    }
    if (-not $answer) { $answer = $it.Def }
    if (-not $answer) { [Console]::Error.WriteLine("'$key' needs: $($it.Ask)"); return 2 }
  }
  $bin = PickBin $words
  $argv = @($words | ForEach-Object { if ($_ -eq "{}") { $answer } else { $_ } })
  if ($script:dry) { "$bin " + (($argv | ForEach-Object { "'$_'" }) -join " "); return 0 }
  [Console]::Error.WriteLine("-> noskill $($argv -join ' ')")
  & $bin @argv
  return $LASTEXITCODE
}

function Menu {
  while ($true) {
    ""; "noskill: what do you want to do?"
    $n = 0; $g = ""
    foreach ($it in $items) {
      $n++
      if ($it.Group -ne $g) { ""; "  $($it.Group)"; $g = $it.Group }
      "   {0,2}  {1,-12} {2}" -f $n, $it.Key, $it.Title
    }
    $pick = Read-Host "`nNumber or name (h help, q quit)"
    if (-not $pick -or $pick -in "q", "quit", "exit") { return }
    if ($pick -in "h", "help", "?") { Usage; continue }
    $key = if ($pick -match "^\d+$") { ($items | Select-Object -Skip ([int]$pick - 1) -First 1).Key } else { $pick }
    if (-not $key) { "no such item"; continue }
    try { [void](RunItem $key @()) } catch { [Console]::Error.WriteLine($_.Exception.Message) }
    [void](Read-Host "`nPress Enter for the menu")
  }
}

$rest = @($args)
while ($rest.Count -gt 0 -and $rest[0].StartsWith("-")) {
  switch ($rest[0]) {
    { $_ -in "-h", "--help" } { Usage; exit 0 }
    { $_ -in "-l", "--list" } { List; exit 0 }
    "--source" { $script:fromSource = $true }
    { $_ -in "-n", "--dry" } { $script:dry = $true }
    default { [Console]::Error.WriteLine("unknown option: $($rest[0]) (.\start.cmd --help)"); exit 2 }
  }
  $rest = @($rest | Select-Object -Skip 1)
}
if ($rest.Count -gt 0 -and $rest[0] -eq "help") { Usage; exit 0 }
if ($rest.Count -gt 0) { exit (RunItem $rest[0] @($rest | Select-Object -Skip 1)) }
if ([Console]::IsInputRedirected) { Usage } else { Menu }
