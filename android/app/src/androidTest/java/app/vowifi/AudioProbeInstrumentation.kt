package app.vowifi

import android.Manifest
import android.app.Instrumentation
import android.app.Activity
import android.bluetooth.BluetoothManager
import android.bluetooth.BluetoothAdapter
import android.bluetooth.BluetoothProfile
import android.content.pm.PackageManager
import android.media.AudioManager
import android.os.Bundle
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/** Read-only device capability probe; never records, pairs or changes audio routing. */
object AudioDeviceProbe {
    fun run(instrumentation: Instrumentation) {
        val report = Bundle()
        val ctx = instrumentation.targetContext
        val granted = ctx.checkSelfPermission(Manifest.permission.BLUETOOTH_CONNECT) == PackageManager.PERMISSION_GRANTED
        report.putBoolean("bluetooth_permission", granted)
        val am = ctx.getSystemService(AudioManager::class.java)
        val comm = am.availableCommunicationDevices
        report.putString("communication_types", comm.map { "type=${it.type},addressLength=${it.address.length},validAddress=${BluetoothAdapter.checkBluetoothAddress(it.address)}" }.toString())
        if (granted) {
            val adapter = ctx.getSystemService(BluetoothManager::class.java).adapter
            val latch = CountDownLatch(2)
            val listener = object : BluetoothProfile.ServiceListener {
                override fun onServiceConnected(profile: Int, proxy: BluetoothProfile) {
                    val list = proxy.connectedDevices.map { device ->
                        val types = comm.filter { it.address.equals(device.address, true) }.map { it.type }
                        "class=${device.bluetoothClass?.deviceClass},major=${device.bluetoothClass?.majorDeviceClass},communicationTypes=$types,nameMatchTypes=${comm.filter { it.productName.toString().equals(device.name, true) }.map { it.type }}"
                    }
                    synchronized(report) { report.putString("profile_$profile", list.toString()) }
                    adapter.closeProfileProxy(profile, proxy); latch.countDown()
                }
                override fun onServiceDisconnected(profile: Int) {}
            }
            for (profile in listOf(BluetoothProfile.HEADSET, BluetoothProfile.A2DP)) {
                if (!adapter.getProfileProxy(ctx, listener, profile)) latch.countDown()
            }
            latch.await(5, TimeUnit.SECONDS)
        }
        instrumentation.finish(Activity.RESULT_OK, report)
    }
}
