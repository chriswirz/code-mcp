@echo off
REM Convenience wrapper that runs deploy-ubuntu.ps1 (cross-compile + install
REM code-mcp.service on the target machine).
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0deploy-ubuntu.ps1" %*
exit /b %ERRORLEVEL%
