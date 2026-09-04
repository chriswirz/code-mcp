# Deploys code-mcp to an Ubuntu machine (linux-arm64 by default) following the
# install instructions in the header of deploy/code-mcp.service:
#   - cross-compiles dist/codemcp-linux-arm64 if it is not already there
#   - copies the binary and the systemd unit to the device
#   - creates the service user and /opt/code-mcp, plus the workspace directory
#   - installs the unit with the service user, workspace, URL and token
#     substituted in, reloads systemd, and makes sure the service is enabled
#     and running
#
# No config file is deployed. Everything the device needs is on the ExecStart
# line, so a redeploy cannot clobber a config.json placed on the machine by
# hand - the server reads one from /opt/code-mcp if it is there.
#
# Uses PuTTY's pscp/plink for password automation when -Password is given and
# they are installed; otherwise falls back to OpenSSH scp/ssh, which is the
# right path when you have a key on the device (you may be prompted a few
# times if you do not).
#
# If Windows Defender blocks the cross-compile ("the file contains a virus"),
# it is flagging the freshly linked ELF, not this repository: add the go build
# cache and the output directory to its exclusions, or build the binary on the
# device and skip the build step by staging dist\codemcp-linux-arm64 yourself.
#
# Usage:  ./deploy-ubuntu.ps1 [-TargetHost 192.168.0.162] [-User ubuntu]
#                             [-Password secret] [-Arch arm64] [-AuthToken t]
#                             [-AllowOrigin https://example.com]

param(
    [string]$TargetHost = '192.168.0.162',
    [string]$User = 'ubuntu',
    [string]$Password = '',
    [ValidateSet('arm64', 'amd64')]
    [string]$Arch = 'arm64',
    [string]$ServiceUser = 'codemcp',
    [string]$WorkspaceRoot = '/srv/code-mcp/workspace',
    [string]$ListenURL = '',
    [string]$AuthToken = '',
    [string]$AllowOrigin = ''
)

if (-not $ListenURL) { $ListenURL = "http://0.0.0.0:8765/mcp" }

$ErrorActionPreference = 'Stop'
$repo = $PSScriptRoot
$binary = Join-Path $repo "dist\codemcp-linux-$Arch"
$unit = Join-Path $repo 'deploy\code-mcp.service'
$remoteDir = '/tmp/code-mcp-deploy'
$target = "$User@$TargetHost"

if (-not (Test-Path $binary)) {
    Write-Host "==> $binary missing; cross-compiling for linux/$Arch ..." -ForegroundColor Cyan
    $version = (& git -C $repo describe --tags --always 2>$null)
    if (-not $version) { $version = '0.1.0000-dev' }
    New-Item -ItemType Directory -Force -Path (Join-Path $repo 'dist') | Out-Null
    $env:GOOS = 'linux'; $env:GOARCH = $Arch; $env:CGO_ENABLED = '0'
    try {
        & go build -trimpath -ldflags "-s -w -X main.version=$version" -o $binary $repo
    } finally {
        Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    }
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path $binary)) { throw "build did not produce $binary" }
}
if (-not (Test-Path $unit)) { throw "missing $unit" }

# Stage everything (binary, unit, install script) in one directory so a single
# recursive copy moves it all.
$stage = Join-Path $env:TEMP 'code-mcp-deploy'
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Path $stage | Out-Null
Copy-Item $binary (Join-Path $stage 'codemcp')

# The unit ships with placeholders so the service user, workspace, listen URL
# and auth token are decided here rather than baked into the repository.
$tokenFlag = if ($AuthToken) { " --token $AuthToken" } else { '' }
$originFlag = if ($AllowOrigin) { " --allow-origin $AllowOrigin" } else { '' }
$unitText = (Get-Content $unit -Raw) `
    -replace '__USER__', $ServiceUser `
    -replace '__WORKSPACE__', $WorkspaceRoot `
    -replace '__URL__', $ListenURL `
    -replace '__TOKEN_FLAG__', $tokenFlag `
    -replace '__ORIGIN_FLAG__', $originFlag
[IO.File]::WriteAllText((Join-Path $stage 'code-mcp.service'), ($unitText -replace "`r`n", "`n"))
if (-not $AuthToken) {
    Write-Warning "no -AuthToken: anything that can reach $ListenURL can drive this server"
}

# Remote install steps, per the header of deploy/code-mcp.service. Runs as root
# via sudo -S. LF endings are required.
$installSh = @(
    'set -e',
    "cd $remoteDir",
    "id -u $ServiceUser >/dev/null 2>&1 || useradd --system --home /opt/code-mcp --shell /usr/sbin/nologin $ServiceUser",
    "mkdir -p /opt/code-mcp $WorkspaceRoot",
    '# stop first so the running binary can be replaced',
    'echo "Stopping code-mcp service..." && systemctl stop code-mcp.service 2>/dev/null || true',
    'echo "Installing codemcp..." && install -m 755 codemcp /opt/code-mcp/codemcp',
    '# a symlink so the same binary is on PATH for interactive use',
    'ln -sf /opt/code-mcp/codemcp /usr/local/bin/codemcp',
    '# no config.json is deployed; the ExecStart flags carry the settings, and',
    '# a config.json placed on the device by hand is left exactly as it is',
    "chown -R ${ServiceUser}:${ServiceUser} /opt/code-mcp $WorkspaceRoot",
    'install -m 644 code-mcp.service /etc/systemd/system/code-mcp.service',
    'systemctl daemon-reload',
    'echo "Enabling code-mcp service..." && systemctl enable --now code-mcp.service',
    'systemctl restart code-mcp.service',
    'sleep 2',
    'systemctl is-enabled code-mcp.service',
    'systemctl is-active code-mcp.service',
    'echo "Getting code-mcp status..." && systemctl status code-mcp.service --no-pager -n 15 || true',
    "rm -rf $remoteDir"
) -join "`n"
[IO.File]::WriteAllText((Join-Path $stage 'install.sh'), $installSh + "`n")

# Prefer PuTTY tools when a password was given (they can take it on the command
# line); otherwise OpenSSH, which uses your key.
$plink = Get-Command plink.exe -ErrorAction SilentlyContinue
$pscp = Get-Command pscp.exe -ErrorAction SilentlyContinue
$usePutty = $Password -and $plink -and $pscp

function Invoke-Remote([string]$cmd) {
    if ($usePutty) {
        & plink.exe -ssh -batch -pw $Password $target $cmd
    } else {
        & ssh $target $cmd
    }
    if ($LASTEXITCODE -ne 0) { throw "remote command failed (exit $LASTEXITCODE): $cmd" }
}

if (-not $usePutty) {
    Write-Host "==> Using OpenSSH (ssh/scp). Set -Password with PuTTY installed to automate the password." -ForegroundColor Yellow
}

Write-Host "==> Copying files to ${target}:$remoteDir ..." -ForegroundColor Cyan
Invoke-Remote "rm -rf $remoteDir && mkdir -p $remoteDir"
# pscp/scp copy the staged contents into the (pre-created) remote directory;
# pscp cannot create the target directory itself.
if ($usePutty) {
    & pscp.exe -batch -pw $Password -r "$stage\*" "${target}:$remoteDir/"
} else {
    & scp -r "$stage\*" "${target}:$remoteDir/"
}
if ($LASTEXITCODE -ne 0) { throw "file copy failed (exit $LASTEXITCODE)" }

Write-Host "==> Installing and starting code-mcp.service ..." -ForegroundColor Cyan
if ($usePutty) {
    Invoke-Remote "echo '$Password' | sudo -S bash $remoteDir/install.sh"
} else {
    # No password to feed sudo: it prompts on the terminal, which needs a tty.
    & ssh -t $target "sudo bash $remoteDir/install.sh"
    if ($LASTEXITCODE -ne 0) { throw "remote install failed (exit $LASTEXITCODE)" }
}

Remove-Item -Recurse -Force $stage
Write-Host "==> Deployed. The server is at http://${TargetHost}:8765/mcp" -ForegroundColor Green
Write-Host "    Follow logs with:  ssh $target journalctl -u code-mcp -f" -ForegroundColor Green
if (-not $AuthToken) {
    Write-Host "    Redeploy with -AuthToken <secret> to require a bearer token." -ForegroundColor Green
}
