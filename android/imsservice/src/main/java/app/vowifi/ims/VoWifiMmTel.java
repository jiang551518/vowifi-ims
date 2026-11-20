package app.vowifi.ims;

import android.content.Context;
import android.os.Bundle;
import android.telephony.TelephonyManager;
import android.telephony.ims.ImsCallProfile;
import android.telephony.ims.feature.CapabilityChangeRequest;
import android.telephony.ims.feature.ImsFeature;
import android.telephony.ims.feature.MmTelFeature;
import android.telephony.ims.stub.ImsCallSessionImplBase;
import android.telephony.ims.stub.ImsRegistrationImplBase;

import java.util.List;
import java.util.concurrent.Executor;
import java.util.concurrent.Executors;

/**
 * MMTEL feature for one SIM: voice over WiFi only (IWLAN). Telephony enables
 * the capability when the user turns on WiFi calling in system settings;
 * we then start the app's engine and report registration/capabilities.
 * Call audio is handled on the application processor
 * ({@link #AUDIO_HANDLER_ANDROID}): Telecom uses the VoIP audio mode and the
 * app's RTP/AMR path plays and records it. Emergency calls are never taken
 * (handed back to cellular).
 */
final class VoWifiMmTel extends MmTelFeature {
    final Context ctx;
    final int slotId;
    final int subId;
    final VoWifiRegistration registration;
    final Executor executor;
    final VoWifiSms sms;
    private volatile boolean ready;
    private volatile boolean wifiEnabled;
    private volatile boolean registeredNow;

    VoWifiMmTel(Context ctx, int slotId, int subId, VoWifiRegistration registration) {
        super(Executors.newSingleThreadExecutor());
        this.ctx = ctx.getApplicationContext();
        this.slotId = slotId;
        this.subId = subId;
        this.registration = registration;
        this.executor = Executors.newSingleThreadExecutor();
        this.sms = new VoWifiSms(this);
    }

    @Override
    public android.telephony.ims.stub.ImsSmsImplBase getSmsImplementation() {
        return sms;
    }

    boolean isReady() {
        return ready;
    }

    /**
     * Marks the feature READY. Telephony refuses capability changes (e.g. the
     * WiFi calling switch) until then, and this platform does not call
     * onFeatureReady() on its own, so the service calls this after creation.
     */
    void markReady() {
        if (ready) return;
        ready = true;
        try {
            setFeatureState(ImsFeature.STATE_READY);
        } catch (Throwable t) {
            ImsHub.log("WARN", "设置就绪状态失败: " + t);
        }
        useAndroidAudio();
        Boolean reg = ImsHub.registered.get(subId);
        onRegistration(reg != null && reg, ImsHub.techFor(subId));
        ImsHub.Backend b = ImsHub.backend;
        if (b != null) {
            b.setSmsTakeover(subId, true);
            b.onSubscriptionReady(slotId, subId);
        }
        ImsHub.log("INFO", "卡槽 " + (slotId + 1) + " 的 MMTEL 已就绪");
    }

    @Override
    public void onFeatureReady() {
        markReady();
    }

    @Override
    public void onFeatureRemoved() {
        ready = false;
        ImsHub.features.remove(subId, this);
        ImsHub.Backend b = ImsHub.backend;
        if (b != null) b.setSmsTakeover(subId, false);
        ImsHub.log("INFO", "卡槽 " + (slotId + 1) + " 的 MMTEL 已移除");
    }

    /** Telecom must use the VoIP audio mode: our app records/plays the call audio. */
    void useAndroidAudio() {
        try {
            setCallAudioHandler(AUDIO_HANDLER_ANDROID);
        } catch (Throwable t) {
            // Listener not attached yet; retried when a call starts.
        }
    }

    @Override
    public boolean queryCapabilityConfiguration(int capability, int radioTech) {
        return (capability == MmTelCapabilities.CAPABILITY_TYPE_VOICE
                || capability == MmTelCapabilities.CAPABILITY_TYPE_SMS)
                && (radioTech == ImsRegistrationImplBase.REGISTRATION_TECH_IWLAN
                || radioTech == ImsRegistrationImplBase.REGISTRATION_TECH_CROSS_SIM);
    }

    @Override
    public void changeEnabledCapabilities(CapabilityChangeRequest request, CapabilityCallbackProxy c) {
        Boolean wifi = null;
        for (Object o : request.getCapabilitiesToEnable()) {
            CapabilityChangeRequest.CapabilityPair p = (CapabilityChangeRequest.CapabilityPair) o;
            if (p.getCapability() == MmTelCapabilities.CAPABILITY_TYPE_VOICE
                    && p.getRadioTech() == ImsRegistrationImplBase.REGISTRATION_TECH_IWLAN) wifi = true;
        }
        for (Object o : request.getCapabilitiesToDisable()) {
            CapabilityChangeRequest.CapabilityPair p = (CapabilityChangeRequest.CapabilityPair) o;
            if (p.getCapability() == MmTelCapabilities.CAPABILITY_TYPE_VOICE
                    && p.getRadioTech() == ImsRegistrationImplBase.REGISTRATION_TECH_IWLAN) wifi = false;
        }
        if (wifi == null || wifi == wifiEnabled) return;
        wifiEnabled = wifi;
        ImsHub.log("INFO", "系统设置：WiFi 通话" + (wifi ? "开启" : "关闭") + "（卡槽 " + (slotId + 1) + "）");
        final boolean on = wifi;
        executor.execute(() -> {
            ImsHub.Backend b = ImsHub.backend;
            if (b != null) b.onWifiCallingRequested(slotId, subId, on);
        });
        if (!on) onRegistration(false, ImsRegistrationImplBase.REGISTRATION_TECH_IWLAN);
    }

    /** Engine registration changed: report registration (IWLAN / CROSS_SIM) + voice capability. */
    void onRegistration(boolean isRegistered, int tech) {
        registeredNow = isRegistered;
        registration.report(isRegistered, tech);
        MmTelCapabilities caps = new MmTelCapabilities();
        if (isRegistered) {
            caps.addCapabilities(MmTelCapabilities.CAPABILITY_TYPE_VOICE);
            caps.addCapabilities(MmTelCapabilities.CAPABILITY_TYPE_SMS);
        }
        try {
            notifyCapabilitiesStatusChanged(caps);
        } catch (Throwable t) {
            ImsHub.log("WARN", "上报能力失败: " + t);
        }
    }

    @Override
    public int shouldProcessCall(String[] numbers) {
        // Emergency calls go over cellular (CS / another SIM), never through us.
        try {
            TelephonyManager tm = ctx.getSystemService(TelephonyManager.class);
            for (String n : numbers) {
                if (n != null && tm.isEmergencyNumber(n)) return PROCESS_CALL_CSFB;
            }
        } catch (Throwable t) {
            // fall through
        }
        return registeredNow ? PROCESS_CALL_IMS : PROCESS_CALL_CSFB;
    }

    @Override
    public ImsCallProfile createCallProfile(int serviceType, int callType) {
        return new ImsCallProfile(serviceType, callType);
    }

    @Override
    public ImsCallSessionImplBase createCallSession(ImsCallProfile profile) {
        useAndroidAudio();
        return new VoWifiCallSession(this, profile, null, null);
    }

    /** Engine reported an incoming call: hand it to Telephony. */
    boolean onIncomingCall(String callId, String number) {
        useAndroidAudio();
        VoWifiCallSession s = VoWifiCallSession.incoming(this, callId, number);
        ImsHub.sessions.put(callId, s);
        Bundle extras = new Bundle();
        try {
            notifyIncomingCall(s, callId, extras);
            return true;
        } catch (NoSuchMethodError e) {
            try {
                notifyIncomingCall(s, extras);
                return true;
            } catch (Throwable t) {
                ImsHub.log("WARN", "来电交给系统失败: " + t);
            }
        } catch (Throwable t) {
            ImsHub.log("WARN", "来电交给系统失败: " + t);
        }
        ImsHub.sessions.remove(callId);
        return false;
    }

    @SuppressWarnings("unused")
    static List<?> none() { return null; }
}
