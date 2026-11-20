package app.vowifi.ims;

import android.telephony.ims.ImsService;
import android.telephony.ims.feature.ImsFeature;
import android.telephony.ims.feature.MmTelFeature;
import android.telephony.ims.stub.ImsFeatureConfiguration;
import android.telephony.ims.stub.ImsRegistrationImplBase;

import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

/**
 * System IMS service: Telephony binds it (BIND_IMS_SERVICE) for the SIM slot
 * whose ImsService is overridden to this package
 * ({@code cmd phone ims set-ims-service -s SLOT -c app.vowifi}). It provides
 * MMTEL (voice over WiFi) on top of the app's own SWu/IMS engine, so the
 * system dialer, call log and WiFi calling settings work natively.
 */
public class VoWifiImsService extends ImsService {
    private final Map<Integer, VoWifiRegistration> regBySlot = new ConcurrentHashMap<>();
    private final Map<Integer, VoWifiMmTel> mmtelBySlot = new ConcurrentHashMap<>();

    @Override
    public ImsFeatureConfiguration querySupportedImsFeatures() {
        ImsFeatureConfiguration.Builder b = new ImsFeatureConfiguration.Builder();
        // EMERGENCY_MMTEL is declared too so that, when we take over a slot,
        // the vendor IMS keeps no feature there and stops registering the same
        // number (it otherwise grabs incoming calls). We never place emergency
        // calls over IMS: no CAPABILITY_EMERGENCY_OVER_MMTEL, and
        // shouldProcessCall() sends emergency numbers to cellular (CSFB).
        for (int slot = 0; slot < 2; slot++) {
            b.addFeature(slot, ImsFeature.FEATURE_EMERGENCY_MMTEL);
            b.addFeature(slot, ImsFeature.FEATURE_MMTEL);
        }
        return b.build();
    }

    @Override
    public void readyForFeatureCreation() {
        ImsHub.log("INFO", "已被系统绑定");
    }

    @Override
    public MmTelFeature createMmTelFeatureForSubscription(int slotId, int subId) {
        VoWifiMmTel f = new VoWifiMmTel(this, slotId, subId, registration(slotId));
        VoWifiMmTel old = mmtelBySlot.put(slotId, f);
        if (old != null) ImsHub.features.remove(old.subId, old);
        ImsHub.features.put(subId, f);
        ImsHub.log("INFO", "接管卡槽 " + (slotId + 1) + " 的 MMTEL（subId " + subId + "）");
        // Telephony attaches its listeners right after this returns; report READY then.
        final VoWifiMmTel ready = f;
        new android.os.Handler(android.os.Looper.getMainLooper()).postDelayed(ready::markReady, 500);
        return f;
    }

    @Override
    public MmTelFeature createMmTelFeature(int slotId) {
        // Older call path without a subscription id.
        return createMmTelFeatureForSubscription(slotId, -1);
    }

    @Override
    public ImsRegistrationImplBase getRegistrationForSubscription(int slotId, int subId) {
        return registration(slotId);
    }

    @Override
    public ImsRegistrationImplBase getRegistration(int slotId) {
        return registration(slotId);
    }

    @Override
    public long getImsServiceCapabilities() {
        return 0; // no emergency over MMTEL, no SIP delegates
    }

    VoWifiRegistration registration(int slotId) {
        VoWifiRegistration r = regBySlot.get(slotId);
        if (r == null) {
            r = new VoWifiRegistration(slotId);
            VoWifiRegistration prev = regBySlot.putIfAbsent(slotId, r);
            if (prev != null) r = prev;
        }
        return r;
    }
}
