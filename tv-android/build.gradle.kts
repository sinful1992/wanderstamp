// Wanderstamp TV: a full-screen WebView on the home-network door's /tv page (task 133).
plugins {
	id("com.android.application") version "9.4.1" apply false
}

buildscript {
	dependencies {
		// AGP 9 compiles Kotlin itself; this pins the Kotlin version it uses
		classpath("org.jetbrains.kotlin:kotlin-gradle-plugin:2.4.21")
	}
}
