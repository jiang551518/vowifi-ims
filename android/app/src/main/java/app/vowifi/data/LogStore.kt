package app.vowifi.data

import android.content.Context
import app.vowifi.LogLine
import java.io.File
import java.util.concurrent.Executors

/**
 * Persists the app log to files/logs/app.log (rotated to app.log.1 at
 * [MAX_BYTES]) so it survives app restarts. Lines are tab-separated:
 * epoch-millis, level, message (newlines escaped). Writes happen on a single
 * background thread.
 */
class LogStore(context: Context) {
    private val dir = File(context.filesDir, "logs").apply { mkdirs() }
    private val current = File(dir, "app.log")
    private val previous = File(dir, "app.log.1")
    private val io = Executors.newSingleThreadExecutor { r -> Thread(r, "log-writer").apply { isDaemon = true } }

    /** Reads up to [limit] most recent lines (previous file first). */
    fun load(limit: Int): List<LogLine> {
        val lines = ArrayDeque<LogLine>()
        for (f in listOf(previous, current)) {
            if (!f.exists()) continue
            runCatching {
                f.forEachLine { raw ->
                    val parts = raw.split('\t', limit = 3)
                    val t = parts.getOrNull(0)?.toLongOrNull() ?: return@forEachLine
                    if (parts.size < 3) return@forEachLine
                    lines.addLast(LogLine(t, parts[1], parts[2].replace("\\n", "\n")))
                    if (lines.size > limit) lines.removeFirst()
                }
            }
        }
        return lines.toList()
    }

    fun append(line: LogLine) {
        io.execute {
            runCatching {
                if (current.length() > MAX_BYTES) {
                    previous.delete()
                    current.renameTo(previous)
                }
                current.appendText("${line.time}\t${line.level}\t${line.msg.replace("\n", "\\n")}\n")
            }
        }
    }

    /** Writes immediately on the calling thread (used for crashes, when the process is dying). */
    fun appendNow(line: LogLine) {
        runCatching {
            current.appendText("${line.time}\t${line.level}\t${line.msg.replace("\n", "\\n")}\n")
        }
    }

    /** Directory holding app.log and the captured system.log. */
    val directory: File get() = dir

    fun clear() {
        io.execute {
            current.delete()
            previous.delete()
        }
    }

    /** Total size of the stored log files in bytes. */
    fun sizeBytes(): Long = current.length() + previous.length()

    companion object {
        const val MAX_BYTES = 2L * 1024 * 1024
    }
}
