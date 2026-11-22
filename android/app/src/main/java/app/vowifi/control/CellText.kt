package app.vowifi.control

import org.json.JSONArray
import org.json.JSONObject

/** Turns the root helper's "cellinfo" snapshot into display texts for the WebUI. */
object CellText {
    private const val UNAVAILABLE = Int.MAX_VALUE

    fun format(o: JSONObject): JSONObject {
        val out = JSONObject()
        val reg = o.optInt("reg", 0)
        val inService = reg == 1 || reg == 5 // HOME / ROAMING
        out.put("network", if (!inService) "无服务" else networkName(o.optInt("rat"), o.optInt("nrState", 0)) +
            if (reg == 5 || o.optBoolean("roaming")) " · 漫游" else "")
        if (inService) {
            out.put("band", band(o))
            out.put("signal", signal(o.optJSONArray("signal")))
        }
        return out
    }

    /** TelephonyManager.NETWORK_TYPE_*; nrState 3 = NR connected (5G NSA on an LTE anchor). */
    private fun networkName(rat: Int, nrState: Int): String = when (rat) {
        20 -> "5G NR（独立组网）"
        13, 19 -> if (nrState == 3) "5G NSA（LTE 锚点）" else if (rat == 19) "4G LTE-A" else "4G LTE"
        3, 8, 9, 10, 15 -> "3G WCDMA"
        17 -> "3G TD-SCDMA"
        4, 5, 6, 7, 12, 14 -> "CDMA"
        1, 2, 16 -> "2G GSM"
        18 -> "IWLAN"
        else -> "未知"
    }

    private fun band(o: JSONObject): String {
        val cell = o.optString("cell")
        val bands = o.optJSONArray("bands")?.let { a -> (0 until a.length()).map { a.getInt(it) } }.orEmpty()
        val prefix = when {
            cell.contains("Nr") -> "n"
            cell.contains("Lte") -> "B"
            else -> "Band "
        }
        val channel = listOf("earfcn" to "EARFCN", "nrarfcn" to "NR-ARFCN", "uarfcn" to "UARFCN", "arfcn" to "ARFCN")
            .firstNotNullOfOrNull { (k, label) -> o.optInt(k, UNAVAILABLE).takeIf { it != UNAVAILABLE && it >= 0 }?.let { "$label $it" } }
        val bw = o.optJSONArray("bandwidths")?.let { a -> (0 until a.length()).map { a.getInt(it) } }.orEmpty().filter { it > 0 }
        val bwText = when {
            bw.size > 1 -> "载波聚合 ${bw.joinToString("+") { "${it / 1000}" }} MHz"
            bw.size == 1 -> "${bw[0] / 1000} MHz"
            else -> null
        }
        return listOfNotNull(bands.takeIf { it.isNotEmpty() }?.joinToString("/") { "$prefix$it" }, channel, bwText)
            .joinToString(" · ").ifEmpty { "—" }
    }

    private fun signal(a: JSONArray?): String {
        val s = a?.optJSONObject(0) ?: return "—"
        fun v(k: String) = s.optInt(k, UNAVAILABLE).takeIf { it != UNAVAILABLE }
        val type = s.optString("type")
        val parts = when {
            type.contains("Nr") -> listOfNotNull(v("ssrsrp")?.let { "RSRP $it dBm" }, v("sssinr")?.let { "SINR $it dB" })
            type.contains("Lte") -> listOfNotNull(v("rsrp")?.let { "RSRP $it dBm" }, v("rsrq")?.let { "RSRQ $it dB" },
                v("rssnr")?.let { "SINR $it dB" })
            else -> listOfNotNull(v("dbm")?.let { "$it dBm" })
        }
        val level = s.optInt("level", -1).takeIf { it in 0..4 }?.let { "$it/4 格" }
        return (parts + listOfNotNull(level)).joinToString(" · ").ifEmpty { "—" }
    }
}
