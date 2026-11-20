package app.vowifi.service

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import java.net.URL
import java.util.Locale
import javax.net.ssl.HttpsURLConnection

/**
 * Finds the public IP (and country) that a carrier's ePDG would see for the
 * chosen exit network, using Cloudflare's trace endpoint over that network.
 * "wifi" / "cellular" go out directly (the same proxy bypass the tunnel uses
 * is applied while measuring); "auto" follows the default network, so a VPN
 * or proxy is included. Blocking: call off the main thread.
 */
object ExitIp {
    data class Result(val ip: String, val country: String, val flag: String, val via: String)

    // Cloudflare's trace gives IP + country code. Direct connections from
    // mainland China to 1.1.1.1 are often blocked or hijacked, so the direct
    // modes try ipip.net (reachable in China and abroad) first.
    private const val CF4 = "https://1.1.1.1/cdn-cgi/trace"
    private const val CF6 = "https://[2606:4700:4700::1111]/cdn-cgi/trace"
    private const val IPIP = "https://myip.ipip.net/json"

    fun check(ctx: Context, mode: String, serviceRunning: Boolean): Result {
        val cm = ctx.getSystemService(ConnectivityManager::class.java)
        // While WiFi is connected Android tears mobile data down; ask for the
        // network like the service does and hold it only while measuring.
        var held: ConnectivityManager.NetworkCallback? = null
        val network: Network? = when (mode) {
            "wifi" -> find(cm, NetworkCapabilities.TRANSPORT_WIFI) ?: error("未连接 WiFi")
            "cellular" -> find(cm, NetworkCapabilities.TRANSPORT_CELLULAR)
                ?: request(cm, NetworkCapabilities.TRANSPORT_CELLULAR).also { held = it.second }.first
                ?: error("移动数据不可用（请确认已开启移动数据）")
            else -> cm.activeNetwork ?: error("没有可用网络")
        }
        // When the service runs in this mode it already applied the bypass.
        val tempBypass = mode != "auto" && !serviceRunning
        if (tempBypass) ProxyBypass.apply(true)
        try {
            val via = when (mode) { "wifi" -> "WiFi"; "cellular" -> "移动数据"; else -> viaName(cm, network!!) }
            val order = if (mode == "auto") listOf(CF4, IPIP, CF6) else listOf(IPIP, CF4, CF6)
            val errors = mutableListOf<String>()
            for (url in order) {
                try {
                    val text = fetch(network!!, url)
                    if (url == IPIP) {
                        // {"ret":"ok","data":{"ip":"…","location":["中国","湖南",…]}}
                        val d = org.json.JSONObject(text).getJSONObject("data")
                        val loc = d.optJSONArray("location")
                        val country = loc?.optString(0).orEmpty()
                        val cc = countryCode(country)
                        return Result(d.getString("ip"), country, flag(cc), via)
                    }
                    val kv = text.lines().mapNotNull { l -> l.split('=', limit = 2).takeIf { it.size == 2 } }
                        .associate { it[0] to it[1] }
                    val ip = kv["ip"] ?: continue
                    val cc = kv["loc"].orEmpty().uppercase()
                    val name = if (cc.length == 2) Locale("", cc).getDisplayCountry(Locale.SIMPLIFIED_CHINESE) else ""
                    return Result(ip, name, flag(cc), via)
                } catch (e: Exception) {
                    errors += "${java.net.URI(url).host}: ${e.javaClass.simpleName} ${e.message.orEmpty()}".trim()
                }
            }
            throw IllegalStateException(errors.joinToString("；"))
        } finally {
            if (tempBypass) ProxyBypass.apply(false)
            held?.let { runCatching { cm.unregisterNetworkCallback(it) } }
        }
    }

    /** Requests a network with [transport] and waits up to 10 s for it. */
    private fun request(cm: ConnectivityManager, transport: Int): Pair<Network?, ConnectivityManager.NetworkCallback> {
        val got = java.util.concurrent.LinkedBlockingQueue<Network>(1)
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) { got.offer(network) }
        }
        cm.requestNetwork(NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .addTransportType(transport).build(), cb)
        val n = got.poll(10, java.util.concurrent.TimeUnit.SECONDS)
        if (n == null) runCatching { cm.unregisterNetworkCallback(cb) }
        return n to cb
    }

    private fun find(cm: ConnectivityManager, transport: Int): Network? =
        cm.allNetworks.firstOrNull { n ->
            val c = cm.getNetworkCapabilities(n) ?: return@firstOrNull false
            c.hasTransport(transport) && c.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) &&
                c.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED)
        }

    private fun viaName(cm: ConnectivityManager, n: Network): String {
        val c = cm.getNetworkCapabilities(n) ?: return "默认网络"
        return when {
            c.hasTransport(NetworkCapabilities.TRANSPORT_VPN) -> "VPN"
            c.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "WiFi"
            c.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "移动数据"
            else -> "默认网络"
        }
    }

    private fun fetch(network: Network, url: String): String {
        val conn = network.openConnection(URL(url)) as HttpsURLConnection
        conn.connectTimeout = 6000
        conn.readTimeout = 6000
        conn.useCaches = false
        conn.setRequestProperty("User-Agent", "WiFiCalling")
        return try {
            conn.inputStream.bufferedReader().use { it.readText() }
        } finally {
            conn.disconnect()
        }
    }

    /** ISO code for a country name in Chinese (ipip.net reports names). */
    private fun countryCode(name: String): String =
        if (name.isEmpty()) "" else Locale.getISOCountries().firstOrNull {
            Locale("", it).getDisplayCountry(Locale.SIMPLIFIED_CHINESE) == name
        } ?: ""

    private fun flag(cc: String): String =
        if (cc.length == 2 && cc.all { it in 'A'..'Z' })
            String(Character.toChars(0x1F1E6 + (cc[0] - 'A'))) + String(Character.toChars(0x1F1E6 + (cc[1] - 'A')))
        else ""
}
