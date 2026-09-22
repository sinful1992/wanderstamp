// The app is a thin native shell. The window opens a local launcher page
// (dist/index.html) which asks once for the address of your Wanderstamp
// server and then navigates the WebView to it, so the WebView ends up
// same-origin with the server — sessions, cookies and the photo proxy all
// just work, and updating the site needs no app rebuild.
//
// The address is entered at runtime rather than baked in at build time: the
// APK is published publicly, and a compiled-in address would ship one
// person's server to everyone who downloads it.

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .run(tauri::generate_context!())
        .expect("error while running Wanderstamp");
}
