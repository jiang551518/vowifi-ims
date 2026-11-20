package app.vowifi.ims;

import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

/**
 * Bridge between the system IMS service (this module) and the app's VoWiFi
 * engine. Only plain Java types cross it, so the app never needs the
 * @SystemApi stubs.
 *
 * App -> IMS: registration changes, incoming calls, call state events.
 * IMS -> App: {@link Backend} (start/stop WiFi calling, dial, answer, ...).
 */
public final class ImsHub {
    private ImsHub() {}

    /** Implemented by the app (Core). Calls may come on binder threads. */
    public interface Backend {
        /** The system enabled/disabled WiFi calling (voice over IWLAN) for a SIM. */
        void onWifiCallingRequested(int slot, int subId, boolean enabled);
        /** Starts an outgoing call; returns the engine call id. */
        String dial(int subId, String number) throws Exception;
        void answer(String callId);
        /** Ends, cancels or rejects a call. */
        void hangup(String callId);
        void sendDtmf(String callId, char digit);
        void setMute(boolean mute);
        /** Sends one SMS-SUBMIT TPDU; returns 0 when accepted, else the RP cause. Blocking. */
        int sendSms(int subId, String smsc, byte[] tpdu) throws Exception;
        /** Answers an incoming SMS handed over with {@link #onSmsPdu}. */
        void ackSms(String id, boolean ok, int rpCause);
        /** The system SMS stack owns this SIM's SMS (or no longer does). */
        void setSmsTakeover(int subId, boolean on);
        /** Our MMTEL for this slot's (possibly new, after an eSIM switch) subscription is ready. */
        void onSubscriptionReady(int slot, int subId);
        void log(String level, String msg);
    }

    static volatile Backend backend;
    static final Map<Integer, VoWifiMmTel> features = new ConcurrentHashMap<>();
    static final Map<Integer, VoWifiRegistration> registrations = new ConcurrentHashMap<>();
    static final Map<String, VoWifiCallSession> sessions = new ConcurrentHashMap<>();
    /** Last registration state per subId, replayed when a feature is (re)created. */
    static final Map<Integer, Boolean> registered = new ConcurrentHashMap<>();
    /** Last registration technology per subId (IWLAN or CROSS_SIM). */
    static final Map<Integer, Integer> techs = new ConcurrentHashMap<>();

    static int techFor(int subId) {
        Integer t = techs.get(subId);
        return t != null ? t : android.telephony.ims.stub.ImsRegistrationImplBase.REGISTRATION_TECH_IWLAN;
    }

    public static void setBackend(Backend b) { backend = b; }

    static void log(String level, String msg) {
        Backend b = backend;
        if (b != null) b.log(level, "IMS 服务: " + msg);
    }

    /** True when the system IMS service handles this SIM (the app should not ring/notify itself). */
    public static boolean isActive(int subId) {
        VoWifiMmTel f = features.get(subId);
        return f != null && f.isReady();
    }

    /** True when this engine call belongs to a system (Telephony) call session. */
    public static boolean manages(String callId) {
        return callId != null && sessions.containsKey(callId);
    }

    /**
     * Engine registration state for a SIM. [overCellular]: the tunnel runs over
     * mobile data (reported as cross-SIM registration) rather than WiFi.
     */
    public static void onRegistration(int subId, boolean isRegistered, boolean overCellular) {
        int tech = overCellular ? android.telephony.ims.stub.ImsRegistrationImplBase.REGISTRATION_TECH_CROSS_SIM
                : android.telephony.ims.stub.ImsRegistrationImplBase.REGISTRATION_TECH_IWLAN;
        Boolean prev = registered.put(subId, isRegistered);
        Integer prevTech = techs.put(subId, tech);
        if (prev != null && prev == isRegistered && (!isRegistered || (prevTech != null && prevTech == tech))) return;
        VoWifiMmTel f = features.get(subId);
        if (f != null) f.onRegistration(isRegistered, tech);
        if (isRegistered && (prevTech == null || prevTech != tech || prev == null || !prev)) {
            log("INFO", "向系统报告注册：" + (overCellular ? "跨卡通话（经移动数据）" : "WLAN 通话"));
        }
    }

    /** Hands an incoming call to the system. Returns false if the system does not handle this SIM. */
    public static boolean onIncomingCall(int subId, String callId, String number) {
        VoWifiMmTel f = features.get(subId);
        if (f == null || !f.isReady()) return false;
        return f.onIncomingCall(callId, number);
    }

    /**
     * Incoming SMS for the system SMS app: pdu = SMSC address (TS 23.040) + TPDU.
     * Returns false if the system does not handle this SIM's SMS.
     */
    public static boolean onSmsPdu(int subId, String id, byte[] pdu, boolean statusReport) {
        VoWifiMmTel f = features.get(subId);
        if (f == null || !f.isReady()) return false;
        f.sms.deliver(id, pdu, statusReport);
        return true;
    }

    /**
     * Engine call state: dialing | ringing | incoming | active | ended.
     * For "ended", sipCode / reason describe why.
     */
    public static void onCallState(String callId, String state, int sipCode, String reason, boolean earlyMedia) {
        VoWifiCallSession s = sessions.get(callId);
        if (s != null) s.onEngineState(state, sipCode, reason, earlyMedia);
    }
}
