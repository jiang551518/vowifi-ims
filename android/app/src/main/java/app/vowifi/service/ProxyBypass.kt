package app.vowifi.service

import android.os.Process
import java.io.File
import java.util.concurrent.TimeUnit

/**
 * Lets the WiFi / mobile-data modes really use that network while a root
 * transparent proxy (NetProxy / sing-box TUN with auto_redirect) is running.
 *
 * Such a proxy routes marked traffic into its TUN with a policy rule that
 * outranks Android's per-network rules, so binding a socket to a Network is
 * not enough. sing-box also installs a "bypass" mark (`ip rule … fwmark X/M
 * goto …`) that its own iptables chains and that rule skip. We read that mark
 * from the live rules and set it, via root, on this app's own packets only
 * (iptables owner match, tagged with a comment so it can be removed again).
 * Nothing else in the proxy's configuration is touched.
 */
object ProxyBypass {
    private const val TAG = "vowifi-bypass"

    private fun su(): String =
        listOf("/system/bin/su", "/system/xbin/su", "/sbin/su", "/debug_ramdisk/su").firstOrNull { File(it).exists() } ?: "su"

    /**
     * Enables ([on]) or removes the bypass. Returns a short description for
     * the log, e.g. "已绕过透明代理（mark 0x200000/0xe00000）".
     */
    fun apply(on: Boolean): String {
        val uid = Process.myUid()
        val script = buildString {
            // Remove any rule we added earlier (both families).
            append("for t in iptables ip6tables; do ")
            append("\$t -w -t mangle -S OUTPUT 2>/dev/null | grep -- '--comment $TAG' | sed 's/^-A /-D /' | ")
            append("while read -r r; do eval \"\$t -w -t mangle \$r\"; done; done; ")
            if (on) {
                append("m=\$(ip rule show | grep -m1 -oE 'fwmark 0x[0-9a-f]+/0x[0-9a-f]+ goto' | awk '{print \$2}'); ")
                append("if [ -z \"\$m\" ]; then echo none; exit 0; fi; ")
                append("for t in iptables ip6tables; do ")
                append("\$t -w -t mangle -I OUTPUT 1 -m owner --uid-owner $uid -m comment --comment $TAG -j MARK --set-xmark \$m || echo fail; done; ")
                append("echo \"mark \$m\"")
            } else {
                append("echo off")
            }
        }
        return runCatching {
            val p = ProcessBuilder(su(), "-c", script).redirectErrorStream(true).start()
            val out = p.inputStream.bufferedReader().readText().trim()
            if (!p.waitFor(15, TimeUnit.SECONDS)) { p.destroy(); return "绕过代理：执行超时" }
            when {
                out.contains("fail") -> "绕过代理失败：$out"
                out.startsWith("mark") -> "已让本 App 绕过透明代理（${out.removePrefix("mark ").trim()}），按所选网络直连"
                out == "none" -> "未检测到透明代理，按所选网络直连"
                out == "off" -> "已取消绕过代理"
                else -> "绕过代理：$out"
            }
        }.getOrElse { "绕过代理失败：${it.message}" }
    }
}
