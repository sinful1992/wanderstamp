# Wanderstamp — Android app

A thin [Tauri 2](https://tauri.app) native shell. The app window loads your
Wanderstamp instance directly, so the WebView is **same-origin** with the
server — logins, session cookies and the photo proxy all work with no CORS or
token juggling, and updating the server needs **no app rebuild**. The phone
just needs to be able to reach your server (LAN, VPN such as Tailscale, or a
public reverse proxy).

Not published to any store — you build a signed APK from your own fork and
sideload it onto the family's phones.

## Point it at your server

Edit `src-tauri/tauri.conf.json` and set the window `url` to your instance:

```json
"url": "https://wanderstamp.example.com"
```

Also update the "Try again" link in `dist/index.html` (the offline fallback
page shown when the server is unreachable).

## Building (GitHub Actions — no local toolchain needed)

The heavy Android/Rust build runs in the cloud.

**One-time signing setup** (needed so app *updates* install over the old
version — Android rejects a changed signature):

```bash
cd app && ./make-keystore.sh
```

It prints four values — add them as GitHub repo secrets
(Settings → Secrets and variables → Actions):

| Secret | Value |
|--------|-------|
| `ANDROID_KEYSTORE_BASE64` | the long base64 string it prints |
| `ANDROID_KEYSTORE_PASSWORD` | the password you chose |
| `ANDROID_KEY_ALIAS` | the alias it prints |
| `ANDROID_KEY_PASSWORD` | the same password |

Keep the keystore file backed up and **off git** (already in `.gitignore`) —
losing it means phones must uninstall/reinstall to update.

**Build:** Actions tab → *Build Android APK* → *Run workflow*. Download the
APK from the finished run's artifacts, transfer it to the phone, and open it
(allow "install from this source" once).

## Local build (optional, on a capable machine)

Needs Rust, Android SDK + NDK, JDK 17, and `cargo install tauri-cli`.

```bash
cd app
cargo tauri android init          # generates src-tauri/gen/android (git-ignored)
cargo tauri android dev           # run on a connected device/emulator
cargo tauri android build --apk   # release APK under src-tauri/gen/android/.../outputs/
```
