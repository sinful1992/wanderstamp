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
        .plugin(tauri_plugin_deep_link::init())
        .setup(|app| {
            // A QR link while the app is open: the page on screen is the server's,
            // and only the launcher knows the saved address, so hand it the code.
            #[cfg(mobile)]
            {
                use tauri::Manager;
                use tauri_plugin_deep_link::DeepLinkExt;
                let handle = app.handle().clone();
                app.deep_link().on_open_url(move |event| {
                    let code = event.urls().into_iter().find_map(|u| {
                        let c = u.query()?.strip_prefix("code=")?.to_string();
                        (c.len() == 4 && c.bytes().all(|b| b.is_ascii_uppercase())).then_some(c)
                    });
                    if let (Some(code), Some(win)) = (code, handle.get_webview_window("main")) {
                        let _ = win.eval(&format!(
                            "location.replace('http://tauri.localhost/index.html?pair={code}')"
                        ));
                    }
                });
            }
            let _ = app;
            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running Wanderstamp");
}
