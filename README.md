# Rec-Martori

Înregistrare **martori TV** cu FFmpeg + interfață web. Înlocuiește scripturile `.bat` cu `:loop … goto loop` —
un singur `.exe`, fără dependențe (doar `ffmpeg.exe`).

## Ce face
- Înregistrează oricâte canale în paralel (`-c copy`, segmente la oră fixă, `strftime` în nume) — exact ca scripturile vechi
- **Repornire automată** când ffmpeg cade (pauză configurabilă, implicit 10 s)
- **Watchdog**: dacă nu mai vin date X secunde (implicit 60), omoară și repornește ffmpeg
- **Oprire curată** (trimite `q` la ffmpeg) → MP4-ul se închide corect, nu rămâne corupt
- ffmpeg rulează ascuns, fără ferestre; dacă aplicația e închisă, ffmpeg se închide odată cu ea (fără înregistrări duble)
- Interfață web cu login: stare live, bitrate, viteză, fișier curent, reporniri, uptime, spațiu liber pe disc,
  **imagine live** din sursă, istoric evenimente (când a căzut / a repornit), loguri ffmpeg, comanda exactă
- Fișiere: listă, **redare în browser**, descărcare, ștergere
- Adăugare / editare / ștergere canale din interfață, Start/Stop/Restart per canal sau toate
- Retenție opțională: șterge automat fișierele mai vechi de N zile
- Format MP4 / MKV / TS (TS/MKV rămân redabile și la o cădere bruscă de curent)
- Jurnal permanent în `rec-martori.log`

## Instalare (Windows)
1. Descarcă `Rec-Martori.exe` din **Actions → artifacts** (sau compilează: `go build -o Rec-Martori.exe .`)
2. Pune-l într-un folder fix, ex. `C:\Rec-Martori\`, împreună cu `install-service.bat` și `uninstall-service.bat`
3. Pornește `Rec-Martori.exe`, deschide interfața → **⚙ Setări → 🛡 Instalează ca serviciu** (confirmă UAC pe PC)
   — sau dublu-click pe **`install-service.bat`** (cere drepturi de Administrator) → serviciul **RecMartori** e instalat și pornit
4. Deschide **http://localhost:8090** (implicit `admin` / `admin` — schimbă parola din ⚙ Setări)

Serviciul:
- pornește automat cu Windows, **fără să fie nevoie de utilizator logat**
- dacă procesul cade, Windows îl repornește după 10 s
- la Stop / restart Windows închide curat înregistrările (MP4 valid)
- se vede în `services.msc` ca **Rec-Martori**; dezinstalare: din ⚙ Setări sau `uninstall-service.bat`

Manual, din CMD ca Administrator:
```
Rec-Martori.exe -install -config C:\Rec-Martori\config.json
Rec-Martori.exe -uninstall
sc stop RecMartori  /  sc start RecMartori
```
Fără serviciu (test): `start.bat` sau dublu-click pe exe.

La prima pornire se creează `config.json` cu cele două canale (M9TV, M9 Plus). Calea ffmpeg implicită:
`C:\Users\produ\Documents\ffmpeg\bin\ffmpeg.exe` (modificabilă din Setări).
Serviciul rulează ca *LocalSystem*: are acces la discuri locale (D:\), dar **nu** la unități de rețea mapate
(pentru NAS folosește cale UNC `\\server\share\...` și dă drept de scriere contului calculatorului).

Firewall, dacă vrei acces din rețea:
```
netsh advfirewall firewall add rule name="Rec-Martori" dir=in action=allow protocol=TCP localport=8090
```

## Comanda ffmpeg folosită
```
ffmpeg -hide_banner -rw_timeout 15000000 -i <sursă> -c:v copy -c:a copy -f segment -segment_time 7200
  -segment_atclocktime 1 -reset_timestamps 1 -segment_format mp4 -break_non_keyframes 1 -strftime 1
  D:\Martor\Martor_M9Tv_%Y-%m-%d_%H-%M-%S.mp4
```

## Opțiuni linie de comandă
`-config <cale>` · `-install` · `-uninstall`

## Linux
`go build -o rec-martori . && ./rec-martori` (folosește `ffmpeg` din PATH).
