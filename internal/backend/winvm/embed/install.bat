@echo off
rem runner-warden first-logon script (dockurr/windows runs /oem/install.bat once).
rem Real work is in install.ps1 next to this file; authorized_key.pub is the daemon's key.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" >> C:\rw-install.log 2>&1
exit /b 0
