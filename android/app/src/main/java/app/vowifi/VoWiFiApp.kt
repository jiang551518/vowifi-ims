package app.vowifi

import android.app.Application
import app.vowifi.service.Notifications

class VoWiFiApp : Application() {
    override fun onCreate() {
        super.onCreate()
        Core.init(this)
        Notifications.ensureChannels(this)
        if (!Core.settings.enabled && Core.settings.radioOffSlot != -1) {
            Thread { Core.syncCellularRadio(false) }.start()
        }
    }
}
