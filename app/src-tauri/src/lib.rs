// The app is a thin native shell: the window loads the live Holiday Map site
// over Tailscale (configured as the window URL in tauri.conf.json), so the
// WebView is same-origin with the server — sessions, cookies and the photo
// proxy all just work, and updating the site needs no app rebuild.

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .run(tauri::generate_context!())
        .expect("error while running Holiday Map");
}
