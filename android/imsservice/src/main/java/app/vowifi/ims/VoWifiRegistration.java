package app.vowifi.ims;

import android.telephony.ims.ImsReasonInfo;
import android.telephony.ims.stub.ImsRegistrationImplBase;

/**
 * IMS registration state of one slot, reported to Telephony: IWLAN when the
 * tunnel runs over WiFi, CROSS_SIM when it runs over (the other SIM's) mobile
 * data, which is Android's native term for IMS over another SIM's data.
 */
final class VoWifiRegistration extends ImsRegistrationImplBase {
    final int slotId;
    private volatile Boolean last;
    private volatile int lastTech = -1;

    VoWifiRegistration(int slotId) {
        this.slotId = slotId;
    }

    void report(boolean isRegistered, int tech) {
        if (last != null && last == isRegistered && (!isRegistered || lastTech == tech)) return;
        last = isRegistered;
        lastTech = tech;
        try {
            if (isRegistered) {
                onRegistered(tech);
            } else {
                onDeregistered(new ImsReasonInfo(ImsReasonInfo.CODE_LOCAL_NETWORK_NO_SERVICE, 0, "WiFi calling not registered"));
            }
        } catch (Throwable t) {
            ImsHub.log("WARN", "上报注册状态失败: " + t);
        }
    }

    @Override
    public void triggerFullNetworkRegistration(int sipCode, String sipReason) {
        ImsHub.log("INFO", "系统要求重新注册（" + sipCode + " " + sipReason + "）");
    }
}
