@echo off
setlocal

echo [run.bat] Bringing stack down (if running)...
docker compose down

if errorlevel 1 (
  echo [run.bat] docker compose down returned a non-zero exit code. Continuing anyway.
)

echo [run.bat] Building and starting stack in attached mode...
docker compose up --build

endlocal
