# Wanderstamp TV (Android TV app)

A full-screen WebView on the home-network door's `/tv` page (task 133). The page does the work: pairing code,
trip shelf, journey, photos. This app keeps the screen on, keeps the pairing cookie, sends Back to the page
first (`WSTV.back()`), and shows a "can't reach" screen with automatic retry.

Build (JDK 17+, Android SDK in `ANDROID_HOME`):

    ./gradlew assembleDebug                                      # opens http://192.168.1.15:8097/tv
    ./gradlew assembleDebug -PtvUrl=http://192.168.1.15:8099/tv  # staging

Install: `adb install -r app/build/outputs/apk/debug/app-debug.apk`. Updating keeps the pairing (same signing key);
uninstalling forgets it.
