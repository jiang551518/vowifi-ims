package app.vowifi

import android.app.Activity
import android.app.Instrumentation
import android.app.Notification
import android.app.NotificationManager
import android.content.Intent
import android.os.Bundle
import android.os.SystemClock
import android.view.accessibility.AccessibilityNodeInfo
import android.bluetooth.BluetoothClass
import app.vowifi.call.HeadsetDeviceFilter
import app.vowifi.call.AudioOutput
import app.vowifi.call.CallAudio
import app.vowifi.ui.InCallActivity
import org.json.JSONObject
import java.time.Instant

/** Local UI/notification checks. Never dials, answers or sends SMS. */
class CallSmokeInstrumentation : Instrumentation() {
    private var expectedHeadsets = -1
    private var esimProbe = false
    private var probeOnly = false
    override fun onCreate(arguments: Bundle?) { super.onCreate(arguments); esimProbe = arguments?.getString("esim") == "true"; probeOnly = arguments?.getString("probe") == "true"; expectedHeadsets = arguments?.getString("headsets")?.toIntOrNull() ?: -1; start() }

    private fun find(node: AccessibilityNodeInfo?, text: String): AccessibilityNodeInfo? {
        if (node == null) return null
        if (node.text?.toString() == text || node.contentDescription?.toString() == text) return node
        for (i in 0 until node.childCount) find(node.getChild(i), text)?.let { return it }
        return null
    }

    private fun awaitText(text: String): AccessibilityNodeInfo {
        val until = SystemClock.uptimeMillis() + 6000
        do {
            find(uiAutomation.rootInActiveWindow, text)?.let { return it }
            SystemClock.sleep(100)
        } while (SystemClock.uptimeMillis() < until)
        error("UI text missing: $text")
    }

    private fun click(text: String) {
        var node: AccessibilityNodeInfo? = awaitText(text)
        while (node != null) {
            if (node.isClickable && node.performAction(AccessibilityNodeInfo.ACTION_CLICK)) {
                runCatching { uiAutomation.waitForIdle(200, 3000) }
                return
            }
            node = node.parent
        }
        error("Not clickable: $text")
    }

    override fun onStart() {
        if (esimProbe) { EsimReadProbe.run(this); return }
        if (probeOnly) { AudioDeviceProbe.run(this); return }
        // A bare framework Instrumentation needs the real Application lifecycle.
        runOnMainSync {
            if (runCatching { Core.callHistory }.isFailure)
                callApplicationOnCreate(targetContext.applicationContext as android.app.Application)
        }
        val nm = targetContext.getSystemService(NotificationManager::class.java)
        val prefix = "local-smoke-${System.nanoTime()}"
        val number = "本地通知测试"
        val started = Instant.now().minusSeconds(5)
        val handle = Core::class.java.getDeclaredMethod("handleCall", JSONObject::class.java).apply { isAccessible = true }
        val report = Bundle()
        fun event(suffix: String, incoming: Boolean, answered: Boolean = false, state: String = "ended") {
            val data = JSONObject().put("id", "$prefix-$suffix").put("number", number)
                .put("incoming", incoming).put("state", state).put("started", started.toString())
            if (answered) data.put("answered", Instant.now().toString())
            runOnMainSync { handle.invoke(Core, data) }
        }
        var result = Activity.RESULT_OK
        try {
            check(Core.calls.value.isEmpty()) { "A real call is already running; refusing synthetic checks" }
            check(nm.areNotificationsEnabled()) { "Notification permission is needed for the notification check" }
            val original = Core.callHistory.items.value
            check(original.size < 100) { "History is full; refusing to evict real history for a fixture" }
            event("missed", true)
            val missed = nm.activeNotifications.single { it.tag == "missed:$prefix-missed" }
            check(missed.notification.extras.getString(Notification.EXTRA_TITLE) == "未接来电")
            event("missed", true)
            check(Core.callHistory.items.value.size == original.size + 1) { "Duplicate ended event inserted history twice" }
            missed.notification.contentIntent.send()
            awaitText("全部"); awaitText("未接来电")
            click("拨号键盘")
            missed.notification.contentIntent.send()
            awaitText("全部"); awaitText("未接来电")
            report.putString("missed_notification", "passed: posted, deduplicated, cold/warm navigation to recents")
            click("首页"); click("拨号"); awaitText("ABC")
            missed.notification.contentIntent.send(); awaitText("全部")
            click("拨号"); awaitText("ABC")
            report.putString("dialpad_default", "passed: manual phone-tab navigation always shows keypad, notifications still show recents")
            // Remove immediately, so a full (100-entry) history cannot evict user records on later fixtures.
            Core.callHistory.items.value.filter { it.number == number && it.time == started.toEpochMilli() }.forEach(Core.callHistory::remove)
            event("answered", true, true)
            check(nm.activeNotifications.none { it.tag == "missed:$prefix-answered" })
            Core.callHistory.items.value.filter { it.number == number && it.time == started.toEpochMilli() }.forEach(Core.callHistory::remove)
            event("outgoing", false)
            check(nm.activeNotifications.none { it.tag == "missed:$prefix-outgoing" })
            Core.callHistory.items.value.filter { it.number == number && it.time == started.toEpochMilli() }.forEach(Core.callHistory::remove)
            report.putString("missed_exclusions", "passed: answered and outgoing calls do not notify")

            check(HeadsetDeviceFilter.acceptsDeviceClass(BluetoothClass.Device.AUDIO_VIDEO_HEADPHONES))
            check(HeadsetDeviceFilter.acceptsDeviceClass(BluetoothClass.Device.AUDIO_VIDEO_WEARABLE_HEADSET))
            check(!HeadsetDeviceFilter.acceptsDeviceClass(BluetoothClass.Device.AUDIO_VIDEO_LOUDSPEAKER))
            check(!HeadsetDeviceFilter.acceptsDeviceClass(BluetoothClass.Device.AUDIO_VIDEO_CAR_AUDIO))
            check(!HeadsetDeviceFilter.acceptsDeviceClass(BluetoothClass.Device.AUDIO_VIDEO_HANDSFREE))
            check(!HeadsetDeviceFilter.acceptsDeviceClass(BluetoothClass.Device.WEARABLE_WRIST_WATCH))
            check(!HeadsetDeviceFilter.acceptsDeviceClass(-1))
            report.putString("headset_filter", "passed: headphones accepted, speakers/cars/unknown classes excluded")
            CallAudio.refreshOutputs(targetContext)
            if (expectedHeadsets >= 0) {
                val deadline = SystemClock.uptimeMillis() + 5000
                while (CallAudio.bluetoothOutputs.value.size != expectedHeadsets && SystemClock.uptimeMillis() < deadline) SystemClock.sleep(100)
                check(CallAudio.bluetoothOutputs.value.size == expectedHeadsets) { "Connected headphone list mismatch" }
            }
            report.putInt("connected_headphone_count", CallAudio.bluetoothOutputs.value.size)
            click("拨号键盘")
            click(CallAudio.output.value.label)
            awaitText("音频输出")
            CallAudio.bluetoothOutputs.value.forEach { device ->
                awaitText(device.name + if (device.id == CallAudio.selectedBluetoothOutput.value) " ✓" else "")
            }
            click("扬声器")
            check(CallAudio.output.value == AudioOutput.SPEAKER) { "Dial pad output choice was not retained" }
            click("扬声器")
            click("听筒")
            check(CallAudio.output.value == AudioOutput.EARPIECE)
            report.putString("dialpad_output", "passed: left-side selector retains speaker/earpiece choice before dialing")
            check(AudioOutput.SPEAKER in CallAudio.outputs.value)
            if (AudioOutput.BLUETOOTH !in CallAudio.outputs.value) {
                check(!CallAudio.selectOutput(targetContext, AudioOutput.BLUETOOTH))
            }
            event("ui", false, state = "dialing")
            targetContext.startActivity(Intent(targetContext, InCallActivity::class.java)
                .putExtra("callId", "$prefix-ui").addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            click(CallAudio.output.value.label)
            awaitText("音频输出"); awaitText("扬声器"); click("取消")
            event("ui", false)
            report.putString("audio_selector", "passed: discovers device outputs and opens selector without a call")
            Core.callHistory.items.value.filter { it.number == number && it.time == started.toEpochMilli() }.forEach(Core.callHistory::remove)
            // Go's RTP tests cover media counters using a local in-memory network.
            check(Core.callHistory.items.value.none { it.number == number && it.time == started.toEpochMilli() })
                { "fixture cleanup deferred" }
        } catch (t: Throwable) {
            report.putString("failure", t.toString())
            result = Activity.RESULT_CANCELED
        } finally {
            // Restore only records belonging to this fixture. Keep real history intact.
            Core.calls.value.values.filter { it.id.startsWith(prefix) && it.state != "ended" }.forEach {
                event(it.id.removePrefix("$prefix-"), false)
            }
            Core.callHistory.items.value.filter { it.number == number && it.time == started.toEpochMilli() }.forEach(Core.callHistory::remove)
            listOf("missed", "answered", "outgoing", "ui").forEach { nm.cancel("missed:$prefix-$it", 3) }
            // No real call was started. Clear the fixture's explicit route preference.
            val routing = Class.forName("app.vowifi.call.AudioRouting")
            routing.getMethod("stop").invoke(routing.getField("INSTANCE").get(null))
        }
        finish(result, report)
    }
}
