@echo off
:: Pornește Rec-Martori automat la logarea în Windows (rulează ca Administrator)
cd /d "%~dp0"
schtasks /Create /F /TN "Rec-Martori" /SC ONLOGON /RL HIGHEST /TR "\"%~dp0Rec-Martori.exe\" -config \"%~dp0config.json\""
echo.
echo Gata. Rec-Martori porneste automat la logare.
echo Pentru dezinstalare:  schtasks /Delete /TN "Rec-Martori" /F
pause
