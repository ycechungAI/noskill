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
install|Setup|Build and install noskill (and Go, Ollama, models if missing)|@install||
ollama|Setup|Install Ollama if missing and download the recommended models|@ollama||
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

# Does this computer have Go? If not, offer the official build from go.dev in %LOCALAPPDATA%\Programs\go
# (NOSKILL_GO_DIR to change it): no admin, SHA-256 checked against dl.google.com, folder added to your
# user PATH. On success `go` is on PATH for the rest of this script.
function EnsureGo([bool]$yes) {
  if (Get-Command go -ErrorAction SilentlyContinue) { return $true }
  $gd = if ($env:NOSKILL_GO_DIR) { $env:NOSKILL_GO_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\go" }
  if (Test-Path (Join-Path $gd "bin\go.exe")) { $env:Path = "$gd\bin;$env:Path"; return $true }
  $arch = Arch
  if (-not $arch) { [Console]::Error.WriteLine("install Go yourself: https://go.dev/dl/"); return $false }
  $ver = ((Invoke-WebRequest "https://go.dev/VERSION?m=text" -UseBasicParsing).Content -split "`r?`n")[0].Trim()
  if ($ver -notmatch "^go1\.") { [Console]::Error.WriteLine("couldn't find the current Go version; install Go yourself: https://go.dev/dl/"); return $false }
  if (-not (AskYes $yes "Go isn't installed. Download $ver (about 80 MB, from go.dev, checksum checked) into ${gd}?" $false)) { [Console]::Error.WriteLine("not installed; get Go from https://go.dev/dl/ and run this again"); return $false }
  if (Test-Path $gd) { [Console]::Error.WriteLine("$gd exists but has no bin\go.exe; move it away or set NOSKILL_GO_DIR"); return $false }
  $file = "$ver.windows-$arch.zip"
  $gt = Join-Path ([IO.Path]::GetTempPath()) ("noskill-go-" + [Guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Path $gt | Out-Null
  try {
    [Console]::Error.WriteLine("downloading $file ...")
    $want = ((Invoke-WebRequest "https://dl.google.com/go/$file.sha256" -UseBasicParsing).Content -split "\s+")[0].Trim()
    if (-not (FetchChecked "https://dl.google.com/go/$file" (Join-Path $gt $file) $want)) { return $false }
    Expand-Archive (Join-Path $gt $file) -DestinationPath $gt -Force
    New-Item -ItemType Directory -Path (Split-Path $gd) -Force | Out-Null
    Move-Item (Join-Path $gt "go") $gd
  } finally { Remove-Item $gt -Recurse -Force -ErrorAction SilentlyContinue }   # our own scratch folder
  AddUserPath "$gd\bin"
  [Console]::Error.WriteLine("installed Go ($ver) in $gd and added it to your user PATH (open a new terminal to use it yourself)")
  return $true
}

function Arch {
  if ($env:PROCESSOR_ARCHITEW6432 -eq "ARM64") { return "arm64" }
  switch ($env:PROCESSOR_ARCHITECTURE) { "AMD64" { "amd64" } "ARM64" { "arm64" } default { $null } }
}
function AddUserPath([string]$dir) {
  $env:Path = "$dir;$env:Path"
  $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
  if (($userPath -split ";") -notcontains $dir) { [Environment]::SetEnvironmentVariable("Path", ($(if ($userPath) { "$userPath;" } else { "" }) + $dir), "User") }
}
# AskYes <skip> <question> <defaultYes>: ask on the terminal; false when it can't ask.
function AskYes([bool]$skip, [string]$q, [bool]$def) {
  if ($skip) { return $true }
  if ([Console]::IsInputRedirected) { return $false }
  $a = Read-Host ($q + $(if ($def) { " [Y/n]" } else { " [y/N]" }))
  if ($def) { return ($a -notin "n", "N", "no") } else { return ($a -in "y", "Y", "yes") }
}
# FetchChecked <url> <file> <sha256>: download, then refuse anything that doesn't match the checksum.
function FetchChecked([string]$url, [string]$file, [string]$sha) {
  Invoke-WebRequest $url -OutFile $file -UseBasicParsing
  $got = (Get-FileHash $file -Algorithm SHA256).Hash.ToLower()
  if (-not $sha -or $got -ne $sha.ToLower()) { [Console]::Error.WriteLine("the download doesn't match its checksum; nothing was installed"); return $false }
  return $true
}
function OllamaExe {
  $c = Get-Command ollama -ErrorAction SilentlyContinue
  if ($c) { return $c.Source }
  $p = Join-Path $env:LOCALAPPDATA "Programs\Ollama\ollama.exe"
  if (Test-Path $p) { return $p }
}

# Is Ollama installed? If not, offer to install it (Ollama's own installer, SHA-256 and signature
# checked, per-user) and start it; then offer the models noskill recommends for this much memory.
# Models are fetched one at a time and skipped when already there.
function SetupOllama([bool]$yes) {
  $ob = OllamaExe
  if (-not $ob) {
    $rel = "https://github.com/ollama/ollama/releases/latest/download"
    if (-not (AskYes $yes "Ollama isn't installed. Download its installer (about 1.5 GB, from ollama.com's GitHub release, checksum and signature checked) and run it for you only?" $false)) {
      [Console]::Error.WriteLine("not installed; get Ollama from https://ollama.com/download and run this again"); return $false
    }
    $sums = (Invoke-WebRequest "$rel/sha256sum.txt" -UseBasicParsing).Content -split "`r?`n"
    $want = (($sums | Where-Object { $_ -match "\./OllamaSetup\.exe$" } | Select-Object -First 1) -split "\s+")[0]
    $ot = Join-Path ([IO.Path]::GetTempPath()) ("noskill-ollama-" + [Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $ot | Out-Null
    try {
      $setup = Join-Path $ot "OllamaSetup.exe"
      if (-not (FetchChecked "$rel/OllamaSetup.exe" $setup $want)) { return $false }
      $sig = Get-AuthenticodeSignature $setup
      if ($sig.Status -ne "Valid") { [Console]::Error.WriteLine("the installer's signature isn't valid ($($sig.Status)); nothing was installed"); return $false }
      Start-Process $setup -ArgumentList "/SILENT" -Wait
    } finally { Remove-Item $ot -Recurse -Force -ErrorAction SilentlyContinue }   # our own scratch folder
    $ob = OllamaExe
    if (-not $ob) { [Console]::Error.WriteLine("Ollama didn't install where expected; get it from https://ollama.com/download"); return $false }
    [Console]::Error.WriteLine("installed Ollama: $ob")
  }
  for ($i = 0; $i -lt 40; $i++) { & $ob list *> $null; if ($LASTEXITCODE -eq 0) { break }; Start-Sleep -Seconds 2 }
  if ($LASTEXITCODE -ne 0) { [Console]::Error.WriteLine("Ollama isn't answering yet: start it, then run this again to fetch the models"); return $false }
  $gb = [int]((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory / 1GB)
  $have = (& $ob list) | Select-Object -Skip 1 | ForEach-Object { ($_ -split "\s+")[0] }
  $want = if ($gb -ge 8) { @("granite4:3b", "gemma3:1b") } else { [Console]::Error.WriteLine("This computer has about $gb GB of memory, so only the small model (gemma3:1b) is suggested."); @("gemma3:1b") }
  $todo = @($want | Where-Object { $have -notcontains $_ })
  if ($todo.Count -eq 0) { [Console]::Error.WriteLine("the recommended models are already installed ($($want -join ' '))") }
  elseif (AskYes $yes "Download the recommended models: $($todo -join ' ') (granite4:3b about 2 GB = main, gemma3:1b about 0.8 GB = backup)?" $true) {
    foreach ($m in $todo) { [Console]::Error.WriteLine("-> ollama pull $m"); & $ob pull $m; if ($LASTEXITCODE -ne 0) { [Console]::Error.WriteLine("couldn't fetch ${m}: try  ollama pull $m  later") } }
  }
  return $true
}

# Build this source tree and install it for the current user (no admin), then offer `noskill init`.
# It goes to %LOCALAPPDATA%\Programs\noskill (NOSKILL_INSTALL_DIR to change it). `-y` after the name skips the questions.
# Go and Ollama are fetched first if missing, then the recommended models; each step asks first.
function InstallNoskill([string[]]$more) {
  $yes = $more -contains "-y"
  $dir = if ($env:NOSKILL_INSTALL_DIR) { $env:NOSKILL_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\noskill" }
  $exe = Join-Path $dir "noskill.exe"
  if ($script:dry) { "(in $here) go build -o $exe ./cmd/noskill, then: $exe init"; return 0 }
  if (-not $yes) {
    if ([Console]::IsInputRedirected) { [Console]::Error.WriteLine("run it from a terminal, or add -y:  .\start.cmd install -y"); return 2 }
    $a = Read-Host ("Build noskill and install it to $exe" + $(if (Test-Path $exe) { " (replaces the one already there)" } else { "" }) + "? [y/N]")
    if ($a -notin "y", "Y", "yes") { [Console]::Error.WriteLine("not installed"); return 0 }
  }
  if (-not (EnsureGo $yes)) { return 1 }
  New-Item -ItemType Directory -Path $dir -Force | Out-Null
  $new = "$exe.new"
  Push-Location $here
  try { $env:CGO_ENABLED = "0"; go build -o $new ./cmd/noskill; if ($LASTEXITCODE -ne 0) { return 1 } } finally { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue; Pop-Location }
  if (Test-Path $exe) { Remove-Item "$exe.old" -Force -ErrorAction SilentlyContinue; Move-Item $exe "$exe.old" -Force }   # a running exe can be renamed, not overwritten
  Move-Item $new $exe
  [Console]::Error.WriteLine("installed $exe")
  $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
  if (($userPath -split ";") -notcontains $dir) {
    [Environment]::SetEnvironmentVariable("Path", ($(if ($userPath) { "$userPath;" } else { "" }) + $dir), "User")
    [Console]::Error.WriteLine("added $dir to your user PATH: open a new terminal to use noskill")
  }
  if (-not (SetupOllama $yes)) { [Console]::Error.WriteLine("Ollama and the models can be set up later:  .\start.cmd ollama") }
  $a = if ($yes) { "y" } else { Read-Host 'Run "noskill init" now (finds Ollama and your models, writes a starting config)? [Y/n]' }
  if ($a -in "n", "N", "no") { [Console]::Error.WriteLine("run it later with: noskill init") } else { & $exe init; return $LASTEXITCODE }
  return 0
}

function RunItem([string]$key, [string[]]$more) {
  $it = $items | Where-Object { $_.Key -eq $key }
  if (-not $it) { [Console]::Error.WriteLine("no item called '$key'. .\start.cmd --list shows them."); return 2 }
  $tpl = $it.Tpl
  switch -Regex ($tpl) {
    "^@readme$" { Get-Content (Join-Path $here "docs\START.md"); return 0 }
    "^@install$" { return (InstallNoskill $more) }
    "^@ollama$" {
      if ($script:dry) { "check for Ollama (install it if missing), then ollama pull the recommended models"; return 0 }
      if (SetupOllama ($more -contains "-y")) { return 0 } else { return 1 }
    }
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
