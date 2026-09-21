# Native Windows install for piso.exe.
# Prereqs: Git, Go (see go.mod), Docker Desktop in Linux-containers mode.
# Copies bin + share tree to %LOCALAPPDATA%\piso (no admin) and runs
# `piso setup --rebuild` as the login user so Docker Desktop stays reachable.
$ErrorActionPreference = 'Stop'

function Find-RepoRoot {
    $here = Split-Path -Parent $PSCommandPath
    return (Resolve-Path (Join-Path $here '..')).Path
}

$root = Find-RepoRoot
Set-Location $root

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
    throw 'go not found on PATH. Install Go (see go.mod) and retry.'
}

$docker = Get-Command docker -ErrorAction SilentlyContinue
if (-not $docker) {
    throw 'docker not found on PATH. Install Docker Desktop and switch it to Linux containers.'
}

$osType = (& docker info --format '{{.OSType}}' 2>$null)
if ($osType -and $osType.Trim().ToLower() -ne 'linux') {
    throw "Docker is using '$osType' containers; switch Docker Desktop to Linux containers."
}

Write-Host 'piso: building piso.exe'
New-Item -ItemType Directory -Force -Path (Join-Path $root 'bin') | Out-Null
& go build -o (Join-Path $root 'bin\piso.exe') ./cli/cmd/piso
if ($LASTEXITCODE -ne 0) { throw "go build failed ($LASTEXITCODE)" }

$prefix = Join-Path $env:LOCALAPPDATA 'piso'
$binDir = Join-Path $prefix 'bin'
$share = Join-Path $prefix 'share\piso'
Write-Host "piso: installing to $prefix"
New-Item -ItemType Directory -Force -Path $binDir, $share | Out-Null
Copy-Item -Force (Join-Path $root 'bin\piso.exe') (Join-Path $binDir 'piso.exe')
foreach ($name in @('compose', 'worker', 'gateway')) {
    $dest = Join-Path $share $name
    if (Test-Path $dest) { Remove-Item -Recurse -Force $dest }
    Copy-Item -Recurse (Join-Path $root $name) $dest
}
Copy-Item -Force (Join-Path $root 'go.mod') (Join-Path $share 'go.mod')
Copy-Item -Force (Join-Path $root 'go.sum') (Join-Path $share 'go.sum')
$dockerignore = Join-Path $root '.dockerignore'
if (Test-Path $dockerignore) {
    Copy-Item -Force $dockerignore (Join-Path $share '.dockerignore')
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath) { $userPath = '' }
if ($userPath -notlike "*${binDir}*") {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$binDir", 'User')
    Write-Host "piso: added $binDir to user PATH (open a new terminal)"
}
$env:Path = "$binDir;$env:Path"

$data = Join-Path $env:USERPROFILE '.piso'
Write-Host "piso: setup --rebuild (data $data)"
$env:PISO_DATA = $data
& (Join-Path $binDir 'piso.exe') setup --rebuild
if ($LASTEXITCODE -ne 0) { throw "piso setup --rebuild failed ($LASTEXITCODE)" }

Write-Host @"
installed $binDir\piso.exe
share     $share
data      $data

Next:
  cd \path\to\your\project
  piso up
  piso attach
  piso dashboard

Port 80 is often reserved on Windows; the control port defaults to 8081.
If hosts cannot be written, use http://127.0.0.1:8081
Trust the MITM CA for https://*.piso.local:
  certutil -addstore -user Root $data\ca.crt
"@
