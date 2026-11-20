package app.vowifi.call

import android.annotation.SuppressLint
import android.content.Context
import android.media.AudioAttributes
import android.media.AudioDeviceInfo
import android.media.AudioFocusRequest
import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioRecord
import android.media.AudioTrack
import android.media.MediaCodec
import android.media.MediaFormat
import android.media.MediaRecorder
import android.media.audiofx.AcousticEchoCanceler
import android.media.audiofx.NoiseSuppressor
import app.vowifi.CallInfo
import app.vowifi.Core
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import java.util.concurrent.ArrayBlockingQueue
import java.util.concurrent.TimeUnit

/**
 * Call audio: microphone -> AMR(-WB) encoder -> engine, and
 * engine -> jitter buffer -> AMR(-WB) decoder -> speaker. Frames crossing the
 * engine boundary are in AMR storage format (1 header byte + speech bits).
 */
object CallAudio {
    @Volatile private var session: Session? = null

    private val _muted = MutableStateFlow(false)
    val muted: StateFlow<Boolean> = _muted

    @Synchronized
    fun start(ctx: Context, c: CallInfo) {
        if (session?.callId == c.id) return
        session?.stop()
        _muted.value = false
        session = runCatching { Session(ctx.applicationContext, c).also { it.start() } }
            .onFailure { Core.appendLog("ERROR", "audio start failed: ${it.message}") }
            .getOrNull()
    }

    @Synchronized
    fun stop(id: String) {
        if (session?.callId == id) {
            session?.stop()
            session = null
        }
    }

    fun onFrame(callId: String, frame: ByteArray) {
        session?.takeIf { it.callId == callId }?.push(frame)
    }

    fun setMuted(m: Boolean) { _muted.value = m }

    private class Session(val ctx: Context, call: CallInfo) {
        val callId = call.id
        private val wb = call.codec.equals("AMR-WB", true)
        private val rate = if (wb) 16000 else 8000
        private val frameBytes = rate / 50 * 2
        private val mime = if (wb) MediaFormat.MIMETYPE_AUDIO_AMR_WB else MediaFormat.MIMETYPE_AUDIO_AMR_NB
        private val bitrate = run {
            val wbRates = intArrayOf(6600, 8850, 12650, 14250, 15850, 18250, 19850, 23050, 23850)
            val nbRates = intArrayOf(4750, 5150, 5900, 6700, 7400, 7950, 10200, 12200)
            val t = if (wb) wbRates else nbRates
            val m = if (call.maxMode in t.indices) call.maxMode else t.size - 1
            // 12.65 kbit/s (WB) / 12.2 (NB) are the usual network defaults.
            if (wb) t[minOf(m, 2)] else t[minOf(m, 7)]
        }
        private val queue = ArrayBlockingQueue<ByteArray>(64)
        @Volatile private var running = false
        private var capture: Thread? = null
        private var playback: Thread? = null
        private val am = ctx.getSystemService(AudioManager::class.java)
        private var focus: AudioFocusRequest? = null
        private val noData = byteArrayOf(0x7C) // FT=15 (NO_DATA), Q=1

        fun push(frame: ByteArray) {
            if (!queue.offer(frame)) { queue.poll(); queue.offer(frame) }
        }

        fun start() {
            running = true
            val attrs = AudioAttributes.Builder().setUsage(AudioAttributes.USAGE_VOICE_COMMUNICATION)
                .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH).build()
            focus = AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN_TRANSIENT).setAudioAttributes(attrs).build()
            am.requestAudioFocus(focus!!)
            // Output routing (earpiece / speaker / Bluetooth) is Telecom's: the
            // system in-call UI switches it while the call runs in VoIP audio mode.
            am.mode = AudioManager.MODE_IN_COMMUNICATION
            capture = Thread(::captureLoop, "call-capture").apply { priority = Thread.MAX_PRIORITY; start() }
            playback = Thread({ playbackLoop(attrs) }, "call-playback").apply { priority = Thread.MAX_PRIORITY; start() }
            Core.appendLog("INFO", "audio started: ${if (wb) "AMR-WB" else "AMR"} ${bitrate / 1000.0} kbit/s")
        }

        fun stop() {
            running = false
            capture?.join(500)
            playback?.join(500)
            focus?.let { am.abandonAudioFocusRequest(it) }
            am.mode = AudioManager.MODE_NORMAL
        }

        @SuppressLint("MissingPermission")
        private fun captureLoop() {
            var rec: AudioRecord? = null
            var enc: MediaCodec? = null
            try {
                val min = AudioRecord.getMinBufferSize(rate, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT)
                rec = AudioRecord(MediaRecorder.AudioSource.VOICE_COMMUNICATION, rate,
                    AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT, maxOf(min, frameBytes * 8))
                if (AcousticEchoCanceler.isAvailable()) AcousticEchoCanceler.create(rec.audioSessionId)?.enabled = true
                if (NoiseSuppressor.isAvailable()) NoiseSuppressor.create(rec.audioSessionId)?.enabled = true
                enc = MediaCodec.createEncoderByType(mime).apply {
                    val f = MediaFormat.createAudioFormat(mime, rate, 1)
                    f.setInteger(MediaFormat.KEY_BIT_RATE, bitrate)
                    f.setInteger(MediaFormat.KEY_MAX_INPUT_SIZE, frameBytes * 4)
                    configure(f, null, null, MediaCodec.CONFIGURE_FLAG_ENCODE)
                    start()
                }
                rec.startRecording()
                val pcm = ByteArray(frameBytes)
                val info = MediaCodec.BufferInfo()
                var pts = 0L
                while (running) {
                    var n = 0
                    while (n < frameBytes && running) {
                        val r = rec.read(pcm, n, frameBytes - n)
                        if (r <= 0) break
                        n += r
                    }
                    if (!running) break
                    if (muted.value) pcm.fill(0)
                    val ii = enc.dequeueInputBuffer(20_000)
                    if (ii >= 0) {
                        enc.getInputBuffer(ii)!!.apply { clear(); put(pcm, 0, frameBytes) }
                        enc.queueInputBuffer(ii, 0, frameBytes, pts, 0)
                        pts += 20_000
                    }
                    while (true) {
                        val oi = enc.dequeueOutputBuffer(info, 0)
                        if (oi < 0) break
                        if (info.size > 0 && info.flags and MediaCodec.BUFFER_FLAG_CODEC_CONFIG == 0) {
                            val out = ByteArray(info.size)
                            enc.getOutputBuffer(oi)!!.apply { position(info.offset); get(out) }
                            Core.engine.pushAudio(callId, out)
                        }
                        enc.releaseOutputBuffer(oi, false)
                    }
                }
            } catch (t: Throwable) {
                Core.appendLog("ERROR", "capture: ${t.message}")
            } finally {
                runCatching { rec?.stop() }
                runCatching { rec?.release() }
                runCatching { enc?.stop() }
                runCatching { enc?.release() }
            }
        }

        private fun playbackLoop(attrs: AudioAttributes) {
            var track: AudioTrack? = null
            var dec: MediaCodec? = null
            try {
                val min = AudioTrack.getMinBufferSize(rate, AudioFormat.CHANNEL_OUT_MONO, AudioFormat.ENCODING_PCM_16BIT)
                track = AudioTrack.Builder()
                    .setAudioAttributes(attrs)
                    .setAudioFormat(AudioFormat.Builder().setSampleRate(rate)
                        .setChannelMask(AudioFormat.CHANNEL_OUT_MONO).setEncoding(AudioFormat.ENCODING_PCM_16BIT).build())
                    .setTransferMode(AudioTrack.MODE_STREAM)
                    .setBufferSizeInBytes(maxOf(min, frameBytes * 6))
                    .build()
                dec = MediaCodec.createDecoderByType(mime).apply {
                    configure(MediaFormat.createAudioFormat(mime, rate, 1), null, null, 0)
                    start()
                }
                track.play()
                // Pre-buffer ~60 ms to absorb network jitter.
                val t0 = System.currentTimeMillis()
                while (running && queue.size < 3 && System.currentTimeMillis() - t0 < 300) Thread.sleep(5)
                val info = MediaCodec.BufferInfo()
                var pts = 0L
                val pcm = ByteArray(frameBytes * 4)
                while (running) {
                    // Keep latency bounded.
                    while (queue.size > 12) queue.poll()
                    val frame = queue.poll(25, TimeUnit.MILLISECONDS) ?: noData
                    val ii = dec.dequeueInputBuffer(20_000)
                    if (ii >= 0) {
                        dec.getInputBuffer(ii)!!.apply { clear(); put(frame) }
                        dec.queueInputBuffer(ii, 0, frame.size, pts, 0)
                        pts += 20_000
                    }
                    while (true) {
                        val oi = dec.dequeueOutputBuffer(info, 5_000)
                        if (oi < 0) break
                        if (info.size > 0) {
                            val n = minOf(info.size, pcm.size)
                            dec.getOutputBuffer(oi)!!.apply { position(info.offset); get(pcm, 0, n) }
                            track.write(pcm, 0, n) // blocking: paces playback in real time
                        }
                        dec.releaseOutputBuffer(oi, false)
                    }
                }
            } catch (t: Throwable) {
                Core.appendLog("ERROR", "playback: ${t.message}")
            } finally {
                runCatching { track?.stop() }
                runCatching { track?.release() }
                runCatching { dec?.stop() }
                runCatching { dec?.release() }
            }
        }
    }
}
