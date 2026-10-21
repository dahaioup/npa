@echo off
rem NPA launcher (Windows)
rem
rem Default: run in this console, output goes to the console and to the log file.
rem If the hide_console marker exists (enabled in Web settings), NPA is started
rem detached in a hidden window: output goes to the log file only, and this
rem console can be closed right after startup.
rem
rem Usage: run.bat [--data-dir DIR] [--log-level LEVEL]

setlocal enabledelayedexpansion

set DIR=%~dp0
set ARCH=%PROCESSOR_ARCHITECTURE%
if defined PROCESSOR_ARCHITEW6432 set ARCH=%PROCESSOR_ARCHITEW6432%

set GOARCH=386
if /I "%ARCH%"=="AMD64" set GOARCH=amd64
if /I "%ARCH%"=="ARM64" set GOARCH=arm64
if /I "%ARCH%"=="x86" set GOARCH=386

set BIN=%DIR%bin\npa-windows-%GOARCH%.exe
if not exist "%BIN%" (
  if exist "%DIR%npa.exe" (set BIN=%DIR%npa.exe)
)
if not exist "%BIN%" (
  echo Missing binary: %DIR%bin\npa-windows-%GOARCH%.exe
  exit /b 1
)

set DATA_DIR=%USERPROFILE%\.npa
set PREV=
for %%A in (%*) do (
  if /I "!PREV!"=="--data-dir" set DATA_DIR=%%~A
  set PREV=%%A
)
set MARKER=%DATA_DIR%\hide_console
set LOG=%DATA_DIR%\npa.log

if exist "%MARKER%" (
  if not exist "%DATA_DIR%" mkdir "%DATA_DIR%"
  start "" /b powershell -NoProfile -WindowStyle Hidden -Command "Start-Process -WindowStyle Hidden -FilePath '%BIN%' -ArgumentList '%*'"
  echo NPA started in background; you can close this console.
  echo Log: %LOG%
  echo Web: http://127.0.0.1:56700
  exit /b 0
)

echo NPA starting: %GOARCH% build ^(foreground, output goes to console and %LOG%^)
echo Web: http://127.0.0.1:56700
echo MCP: http://127.0.0.1:56701/mcp
"%BIN%" %*
