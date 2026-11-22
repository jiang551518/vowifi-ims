package app.vowifi

import android.app.Application
import app.vowifi.service.Notifications

class VoWiFiApp : Application() {
    override fun onCreate() {
        super.onCreate()
        // Only the main user's instance does anything: ColorOS's app-clone user
        // (999) also gets a copy of this persistent system app.
        if (android.os.Process.myUid() / 100_000 != 0) return
        Core.init(this)
        Notifications.ensureChannels(this)
        if (!Core.settings.enabled && Core.settings.radioOffSlot != -1) {
            Thread { Core.syncCellularRadio(false) }.start()
        }
    }
}
