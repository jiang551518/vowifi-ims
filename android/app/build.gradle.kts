plugins {
    id("com.android.application")
}

android {
    namespace = "app.vowifi"
    compileSdk = 37

    defaultConfig {
        applicationId = "app.vowifi"
        minSdk = 31
        targetSdk = 36
        versionCode = 4
        versionName = "0.4.0"
        testInstrumentationRunner = "app.vowifi.CallSmokeInstrumentation"
        ndk { abiFilters += "arm64-v8a" }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            signingConfig = signingConfigs.getByName("debug")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    // Native libraries stay uncompressed in the APK and load from it directly:
    // a system app (installed by the module) never gets them extracted.
    packaging { jniLibs { useLegacyPackaging = false } }
}

dependencies {
    implementation(files("libs/vowificore.aar"))
    implementation(project(":imsservice"))
    implementation("androidx.core:core-ktx:1.19.1")
    implementation("androidx.lifecycle:lifecycle-service:2.11.0")
}
