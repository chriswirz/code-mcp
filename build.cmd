@echo off
setlocal enabledelayedexpansion
rem Build codemcp. With no arguments it builds for this machine into
rem codemcp.exe; pass --all to cross-compile every release target into dist\,
rem which is what the GitHub Actions workflow does.

cd /d "%~dp0"

rem Release tags are the version itself (0.1.0042), so describe yields that tag
rem or "0.1.0042-3-gabc1234" a few commits later.
for /f "delims=" %%v in ('git describe --tags --always 2^>nul') do set VERSION=%%v
if "%VERSION%"=="" set VERSION=0.1.0000-dev
set LDFLAGS=-s -w -X main.version=%VERSION%

if "%~1"=="--help" goto help
if "%~1"=="-h" goto help
if "%~1"=="--test" goto test
if "%~1"=="--all" goto all
if "%~1"=="" goto host
echo unknown argument: %~1 (try --help)>&2
exit /b 1

:host
echo Building codemcp %VERSION%
set CGO_ENABLED=0
go build -trimpath -ldflags "%LDFLAGS%" -o codemcp.exe .
if errorlevel 1 exit /b 1
echo Wrote codemcp.exe
goto :eof

:all
echo Building codemcp %VERSION% for all targets
if not exist dist mkdir dist
set CGO_ENABLED=0
call :one windows amd64 .exe
call :one windows arm64 .exe
call :one linux   amd64
call :one linux   arm64
call :one darwin  amd64
call :one darwin  arm64
where certutil >nul 2>&1 && call :sums
goto :eof

:one
set GOOS=%~1
set GOARCH=%~2
set OUT=dist\codemcp-%~1-%~2%~3
echo   %OUT%
go build -trimpath -ldflags "%LDFLAGS%" -o "%OUT%" .
if errorlevel 1 exit /b 1
goto :eof

:sums
if exist dist\SHA256SUMS del dist\SHA256SUMS
for %%f in (dist\codemcp-*) do (
  for /f "skip=1 tokens=* delims=" %%h in ('certutil -hashfile "%%f" SHA256') do (
    if not defined DONE_%%~nf (
      set DONE_%%~nf=1
      echo %%h  %%~nxf>>dist\SHA256SUMS
    )
  )
)
echo Wrote dist\SHA256SUMS
goto :eof

:test
gofmt -l .
go vet ./...
if errorlevel 1 exit /b 1
go test ./...
if errorlevel 1 exit /b 1
goto :eof

:help
echo Usage: build.cmd [--all ^| --test ^| --help]
echo   (no args)  build codemcp.exe for this machine
echo   --all      cross-compile every release target into dist\
echo   --test     gofmt, go vet and go test
goto :eof
