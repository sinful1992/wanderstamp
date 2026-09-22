# Wanderstamp — Android app

A thin [Tauri 2](https://tauri.app) native shell. On first run it asks for the
address of your Wanderstamp server, then loads it directly, so the WebView is
**same-origin** with the server — logins, session cookies and the photo proxy
all work with no CORS or token juggling, and updating the server needs **no app
rebuild**. The phone just needs to be able to reach your server (LAN, VPN such
as Tailscale, or a public reverse proxy).

Not on any store: download the APK from the
[latest release](../../releases) and sideload it, or build your own below.

## The server address is entered, not built in

The launcher page (`dist/index.html`) is what the window opens. It keeps the
address in the app's own storage on the phone, so one APK works for everyone —
nothing about your server is compiled into a published build.

- **Wrong address, or the server moved?** Whenever the app can't reach it, the
  launcher comes back with *Try again* and *Use a different address*.
- **Server reachable but you want to switch?** Android's back button returns to
  the launcher from the site.

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

**Release:** push a tag like `app-v1.1.0` and the same workflow publishes the
signed APK as a GitHub release. Server image tags (`v1.8.1`) are a separate
line and build the Docker image instead.

> On a **public** repository, both run logs and build artifacts are readable by
> anyone. Never pass a private address through a repo variable (variables appear
> in logs verbatim; secrets are masked) and never bake one into an APK built
> here — the artifact ships it to everyone.

## Renaming a fork

Name and application id are source, not build settings — edit
`app/src-tauri/tauri.conf.json` (`productName`, `identifier`, the window
`title`). Keep `identifier` stable once phones have installed it, or the next
APK installs beside the old app instead of upgrading it.

## Local build (optional, on a capable machine)

Needs Rust, Android SDK + NDK, JDK 17, and `cargo install tauri-cli`.

```bash
cd app
cargo tauri android init          # generates src-tauri/gen/android (git-ignored)
cargo tauri android dev           # run on a connected device/emulator
cargo tauri android build --apk   # release APK under src-tauri/gen/android/.../outputs/
```
