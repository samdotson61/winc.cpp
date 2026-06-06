# =============================================================================
# winc - tiny CLI for winc.cpp
#   winc ls                      list downloaded + available models
#   winc -d <alias|repo file>    download a model (HuggingFace)
#   winc -r <model>              delete a downloaded model
#   winc -s <app> <model>        start a sandboxed local instance with a model
#                                  app = claude | opencode | cli
#   winc help                    this help
# =============================================================================
$ErrorActionPreference = 'Stop'

$Root        = $PSScriptRoot
$ModelsDir   = Join-Path $Root 'models'
$VenvScripts = Join-Path $Root 'venv\Scripts'
$Launcher    = Join-Path $Root 'launcher.ps1'
. (Join-Path $Root 'catalog.ps1')   # -> $WINC_MODELS

function Say  { param($m) Write-Host $m }
function Good { param($m) Write-Host "[+] $m" -ForegroundColor Green }
function Warn { param($m) Write-Host "[!] $m" -ForegroundColor Yellow }
function Die  { param($m) Write-Host "[x] $m" -ForegroundColor Red; exit 1 }

function Resolve-Catalog {
    param($q)
    if (-not $q) { return $null }
    foreach ($m in $WINC_MODELS) {
        if ($m.Alias -ieq $q -or "$($m.N)" -eq $q -or $m.File -ieq $q -or $m.Name -ieq $q) { return $m }
    }
    return $null
}

function Resolve-Downloaded {
    # Find a downloaded .gguf matching an alias/filename/substring. Returns FileInfo or $null.
    param($q)
    if (-not (Test-Path $ModelsDir)) { return $null }
    $entry = Resolve-Catalog $q
    $target = if ($entry) { $entry.File } else { $q }
    $files = Get-ChildItem -Path $ModelsDir -Filter '*.gguf' -File -ErrorAction SilentlyContinue
    foreach ($f in $files) { if ($f.Name -ieq $target -or $f.BaseName -ieq $target) { return $f } }
    foreach ($f in $files) { if ($f.Name -like "*$target*") { return $f } }
    return $null
}

function Find-VenvPython {
    $p = Join-Path $VenvScripts 'python.exe'
    if (Test-Path $p) { return $p }
    return $null
}

function Show-Usage {
    Say ""
    Say "winc - local Claude Code models (winc.cpp)"
    Say ""
    Say "  winc ls                       list downloaded + available models"
    Say "  winc -d <alias>               download a catalogue model"
    Say "  winc -d <repo> <file>         download any GGUF from HuggingFace"
    Say "  winc -r <model>               delete a downloaded model (-y to skip prompt)"
    Say "  winc -s claude <model>        start Claude Code on a local model (sandboxed)"
    Say "  winc -s opencode <model>      start OpenCode on a local model"
    Say "  winc -s cli <model>           start the raw llama.cpp chat CLI"
    Say "  winc help                     show this help"
    Say ""
    Say "  <model> is an alias (see 'winc ls') or part of a downloaded filename."
    Say ""
}

function Cmd-Ls {
    Say ""
    Say "Downloaded (in models\):"
    $files = @()
    if (Test-Path $ModelsDir) { $files = Get-ChildItem -Path $ModelsDir -Filter '*.gguf' -File | Sort-Object Name }
    if ($files.Count -eq 0) {
        Say "  (none yet - use 'winc -d <alias>')"
    } else {
        foreach ($f in $files) {
            $gb = [Math]::Round($f.Length / 1GB, 1)
            Say ("  {0,-46} {1,6} GB" -f $f.Name, $gb)
        }
    }
    Say ""
    Say "Available to download (alias  ~size  model):"
    foreach ($m in $WINC_MODELS) {
        $have = Resolve-Downloaded $m.Alias
        $mark = if ($have) { '[installed]' } else { '' }
        Say ("  {0,-14} {1,8}  {2} {3}" -f $m.Alias, $m.Size, $m.Name, $mark)
        Say ("                          {0}" -f $m.Note)
    }
    Say ""
    Say "Download:  winc -d <alias>      Start:  winc -s claude <alias>"
    Say ""
}

function Cmd-Download {
    param($rest)
    $rest = @($rest)   # never let a single arg arrive as a scalar string
    if (-not $rest -or $rest.Count -eq 0) { Die "Usage: winc -d <alias>   or   winc -d <repo> <file>" }
    $py = Find-VenvPython
    if (-not $py) { Die "venv not found. Run install.cmd first." }

    if ($rest.Count -ge 2) {
        $repo = $rest[0]; $file = $rest[1]
    } else {
        $entry = Resolve-Catalog $rest[0]
        if (-not $entry) { Die "Unknown model '$($rest[0])'. Run 'winc ls' for aliases, or pass '<repo> <file>'." }
        $repo = $entry.Repo; $file = $entry.File
    }

    $target = Join-Path $ModelsDir $file
    if (Test-Path $target) { Good "Already downloaded: $file"; return }
    New-Item -ItemType Directory -Force -Path $ModelsDir | Out-Null
    Good "Downloading $file"
    Say  "  from $repo"
    # Use the venv python + hf_get.py (clean 1s progress bar + ETA), not the
    # hf.exe shim (which breaks if the install folder is renamed).
    & $py (Join-Path $Root 'hf_get.py') $repo $file $ModelsDir
    if ($LASTEXITCODE -ne 0) { Die "Download failed. For gated models set HF_TOKEN (`$env:HF_TOKEN='hf_...') and retry." }
    if (Test-Path $target) { Good "Done: $file" } else { Warn "Download reported success but $file is not in models\ - check the filename." }
}

function Cmd-Start {
    param($rest)
    $rest = @($rest)   # never let args arrive as a scalar string
    if (-not $rest -or $rest.Count -lt 2) { Die "Usage: winc -s <claude|opencode|cli> <model>" }
    $app = "$($rest[0])".ToLower()
    $modelQ = $rest[1]
    $mode = switch ($app) {
        'claude'   { '2' }
        'opencode' { '3' }
        'cli'      { '1' }
        default    { Die "Unknown app '$app'. Use claude, opencode, or cli." }
    }
    if (-not (Test-Path $Launcher)) { Die "launcher.ps1 not found. Run install.cmd first." }

    $file = Resolve-Downloaded $modelQ
    if (-not $file) {
        $entry = Resolve-Catalog $modelQ
        if ($entry) { Die "'$($entry.Alias)' is not downloaded yet. Run:  winc -d $($entry.Alias)" }
        Die "No downloaded model matches '$modelQ'. See 'winc ls'."
    }

    Good "Starting $app on $($file.Name) (sandboxed local instance)"
    $env:WINC_MODEL = $file.Name
    $env:WINC_MODE  = $mode
    try { & $Launcher } finally { Remove-Item Env:\WINC_MODEL, Env:\WINC_MODE -ErrorAction SilentlyContinue }
}

function Cmd-Remove {
    param($rest)
    $rest = @($rest)   # never let a single arg arrive as a scalar string
    if (-not $rest -or $rest.Count -eq 0) { Die "Usage: winc -r <alias|filename>   (add -y to skip the prompt)" }
    $yes = $rest -contains '-y' -or $rest -contains '--yes'
    $q   = @($rest | Where-Object { $_ -ne '-y' -and $_ -ne '--yes' })[0]
    if (-not $q) { Die "Usage: winc -r <alias|filename>" }

    $file = Resolve-Downloaded $q
    if (-not $file) {
        $entry = Resolve-Catalog $q
        if ($entry) { Die "'$($entry.Alias)' is not downloaded - nothing to remove." }
        Die "No downloaded model matches '$q'. See 'winc ls'."
    }
    $gb = [Math]::Round($file.Length / 1GB, 1)
    if (-not $yes) {
        $ans = Read-Host "Delete $($file.Name) ($gb GB)? [y/N]"
        if ($ans -notmatch '^[yY]') { Say "Cancelled - nothing removed."; return }
    }
    Remove-Item $file.FullName -Force
    Good "Removed: $($file.Name) ($gb GB freed)"
}

# -- dispatch ----------------------------------------------------------------
# NOTE: build $rest with an explicit @() wrapper. A one-element slice returned
# from an `if {}` block gets unwrapped to a scalar string, and then $rest[0]
# would index the STRING (e.g. 'qwen3.6-35b'[0] -> 'q'). @() keeps it an array.
$cmd  = ''
$rest = @()
if ($args.Count -ge 1) { $cmd = "$($args[0])".ToLower() }
if ($args.Count -ge 2) { $rest = @($args[1..($args.Count - 1)]) }

switch ($cmd) {
    'ls'        { Cmd-Ls }
    'list'      { Cmd-Ls }
    '-d'        { Cmd-Download $rest }
    'download'  { Cmd-Download $rest }
    '-r'        { Cmd-Remove $rest }
    'rm'        { Cmd-Remove $rest }
    'remove'    { Cmd-Remove $rest }
    '-s'        { Cmd-Start $rest }
    'start'     { Cmd-Start $rest }
    'help'      { Show-Usage }
    '-h'        { Show-Usage }
    '--help'    { Show-Usage }
    ''          { Show-Usage }
    default     { Warn "Unknown command '$cmd'"; Show-Usage; exit 1 }
}
