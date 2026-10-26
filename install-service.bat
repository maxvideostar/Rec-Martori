@echo off
:: Instaleaza Rec-Martori ca serviciu Windows (porneste cu Windows, fara utilizator logat)
net session >nul 2>&1 || (powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs" & exit /b)
cd /d "%~dp0"
Rec-Martori.exe -install -config "%~dp0config.json"
echo.
echo Interfata: http://localhost:8090
pause
