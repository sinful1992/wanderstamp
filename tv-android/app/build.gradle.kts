plugins {
	id("com.android.application")
}

// The /tv page this app opens. Override for staging: ./gradlew assembleDebug -PtvUrl=http://192.168.1.15:8099/tv
val tvUrl = (findProperty("tvUrl") as String?) ?: "http://192.168.1.15:8097/tv"
// res/xml/network_security_config.xml allows plain HTTP to the home server only
check(java.net.URI(tvUrl).host == "192.168.1.15") { "tvUrl must be on 192.168.1.15 (network_security_config): $tvUrl" }

android {
	namespace = "com.wanderstamp.tv"
	compileSdk = 36

	defaultConfig {
		applicationId = "com.wanderstamp.tv"
		minSdk = 23
		targetSdk = 36
		versionCode = 1
		versionName = "1.0.0"
		buildConfigField("String", "TV_URL", "\"$tvUrl\"")
	}

	buildFeatures {
		buildConfig = true
	}

	compileOptions {
		sourceCompatibility = JavaVersion.VERSION_17
		targetCompatibility = JavaVersion.VERSION_17
	}

	buildTypes {
		release {
			isMinifyEnabled = false
		}
	}
}
