package app.vowifi

import android.app.Activity
import android.app.Instrumentation
import android.os.Bundle
import android.se.omapi.SEService
import app.vowifi.esim.EsimReader
import kotlinx.coroutines.runBlocking
import org.json.JSONObject
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/** Uses normal OMAPI authorisation. Reads only; never modifies profiles or access rules. */
object EsimReadProbe {
    private fun hex(s: String) = s.chunked(2).map { it.toInt(16).toByte() }.toByteArray()
    fun run(i: Instrumentation) {
        val result = Bundle()
        val ready = CountDownLatch(1)
        val service = SEService(i.targetContext, i.targetContext.mainExecutor) { ready.countDown() }
        try {
            check(ready.await(8, TimeUnit.SECONDS)) { "OMAPI service timeout" }
            val readers = service.readers.filter { it.name.startsWith("SIM", true) }
            result.putInt("sim_reader_count", readers.size)
            for (reader in readers) {
                val key = reader.name
                try {
                    result.putBoolean("${key}_present", reader.isSecureElementPresent)
                    if (!reader.isSecureElementPresent) continue
                    val session = reader.openSession()
                    try {
                        val channel = session.openLogicalChannel(hex("A0000005591010FFFFFFFF8900000100"))
                        if (channel == null) { result.putString("${key}_access", "channel unavailable"); continue }
                        try {
                            result.putString("${key}_access", "authorised")
                            val eid = channel.transmit(hex("80E2910006BF3E035C015A00"))
                            result.putString("${key}_eid_status", eid.takeLast(2).joinToString("") { "%02X".format(it) })
                            result.putInt("${key}_eid_response_length", eid.size)
                            val info = channel.transmit(hex("80E2910003BF220000"))
                            result.putString("${key}_info_status", info.takeLast(2).joinToString("") { "%02X".format(it) })
                            result.putInt("${key}_info_response_length", info.size)
                        } finally { channel.close() }
                    } finally { session.close() }
                } catch (e: Exception) {
                    // No card identifiers or APDU bodies in diagnostics.
                    result.putString("${key}_access", e.javaClass.simpleName)
                }
            }
            val data = runBlocking { JSONObject(EsimReader.read(i.targetContext, 1)) }
            val info = data.getJSONObject("info")
            val profiles = data.getJSONArray("profiles")
            result.putInt("parsed_eid_length", info.getString("eid").length)
            result.putString("parsed_sgp22", info.optString("sgp22Version"))
            result.putString("parsed_firmware", info.optString("firmwareVersion"))
            result.putInt("parsed_profile_count", profiles.length())
            result.putInt("parsed_enabled_count", (0 until profiles.length()).count { profiles.getJSONObject(it).optBoolean("enabled") })
            check(info.getString("eid").length == 32)
            check(profiles.length() > 0)
            check(data.getBoolean("readOnly"))
            i.finish(Activity.RESULT_OK, result)
        } catch(e: Exception) {
            result.putString("error", e.javaClass.simpleName)
            i.finish(Activity.RESULT_CANCELED, result)
        } finally { service.shutdown() }
    }
}
