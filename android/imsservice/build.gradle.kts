import java.util.Properties

// System IMS service (android.telephony.ims.ImsService). Those APIs are
// @SystemApi: this module compiles against stubs generated from the device
// framework (libs/ims-stubs.jar, see tools/gen_ims_stubs.py) placed before
// android.jar on the boot class path. The stubs are not packaged; the device
// framework provides the real classes (the app runs as a system app).
plugins {
    `java-library`
}

java {
    sourceCompatibility = JavaVersion.VERSION_1_8
    targetCompatibility = JavaVersion.VERSION_1_8
}

val sdkDir: String = Properties().apply {
    rootProject.file("local.properties").takeIf { it.exists() }?.inputStream()?.use { load(it) }
}.getProperty("sdk.dir") ?: System.getenv("ANDROID_HOME") ?: error("Android SDK not found")

tasks.withType<JavaCompile>().configureEach {
    options.bootstrapClasspath = files("libs/ims-stubs.jar", "$sdkDir/platforms/android-37.0/android.jar")
    options.encoding = "UTF-8"
    options.compilerArgs.addAll(listOf("-Xlint:-options", "-nowarn"))
}
