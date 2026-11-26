package app.vowifi.ims;

import android.os.Message;
import android.telephony.ims.ImsCallProfile;
import android.telephony.ims.ImsCallSessionListener;
import android.telephony.ims.ImsReasonInfo;
import android.telephony.ims.ImsStreamMediaProfile;
import android.telephony.ims.stub.ImsCallSessionImplBase;

/**
 * One call as seen by Telephony, backed by an engine call. Outgoing: Telephony
 * creates the session and calls {@link #start}; incoming: created from the
 * engine event and handed over with notifyIncomingCall.
 */
final class VoWifiCallSession extends ImsCallSessionImplBase {
    // ImsCallSessionImplBase.State values.
    private static final int IDLE = 0, INITIATED = 1, ESTABLISHING = 3, ESTABLISHED = 4,
            TERMINATING = 7, TERMINATED = 8;
    private static final int VIDEO_QUALITY_NONE = 0, DIRECTION_INVALID = -1;

    private final VoWifiMmTel feature;
    private volatile ImsCallProfile profile;
    private volatile ImsCallSessionListener listener;
    private volatile String callId;
    private volatile int state = IDLE;
    private final boolean incoming;
    private volatile boolean established;

    VoWifiCallSession(VoWifiMmTel feature, ImsCallProfile profile, String callId, String number) {
        this.feature = feature;
        this.profile = profile;
        this.callId = callId;
        this.incoming = callId != null;
    }

    static VoWifiCallSession incoming(VoWifiMmTel f, String callId, String number) {
        ImsCallProfile p = new ImsCallProfile(ImsCallProfile.SERVICE_TYPE_NORMAL, ImsCallProfile.CALL_TYPE_VOICE,
                new android.os.Bundle(), media(ImsStreamMediaProfile.DIRECTION_SEND_RECEIVE));
        String n = number == null ? "" : number;
        p.setCallExtra(ImsCallProfile.EXTRA_OI, n);
        p.setCallExtraInt(ImsCallProfile.EXTRA_OIR,
                n.isEmpty() ? 1 /* restricted */ : ImsCallProfile.OIR_PRESENTATION_NOT_RESTRICTED);
        p.setCallExtraInt(ImsCallProfile.EXTRA_CNAP, ImsCallProfile.OIR_PRESENTATION_NOT_RESTRICTED);
        VoWifiCallSession s = new VoWifiCallSession(f, p, callId, n);
        s.state = INITIATED;
        return s;
    }

    private static ImsStreamMediaProfile media(int audioDirection) {
        // The 5-argument constructor is the public SystemApi one; the 4-argument
        // variant is hidden and blocked at run time (NoSuchMethodError on a 183).
        return new ImsStreamMediaProfile(ImsStreamMediaProfile.AUDIO_QUALITY_AMR_WB, audioDirection,
                VIDEO_QUALITY_NONE, DIRECTION_INVALID, 0 /* RTT_MODE_DISABLED */);
    }

    private ImsCallSessionListener l() {
        return listener;
    }

    // ------------------------------------------------------------ Telephony -> us

    @Override
    public void setListener(ImsCallSessionListener listener) {
        this.listener = listener;
    }

    @Override
    public String getCallId() {
        String id = callId;
        return id == null ? "" : id;
    }

    @Override
    public ImsCallProfile getCallProfile() {
        return profile;
    }

    @Override
    public ImsCallProfile getLocalCallProfile() {
        return profile;
    }

    @Override
    public ImsCallProfile getRemoteCallProfile() {
        return profile;
    }

    @Override
    public int getState() {
        return state;
    }

    @Override
    public boolean isInCall() {
        return state == ESTABLISHED;
    }

    @Override
    public void start(String callee, ImsCallProfile p) {
        if (p != null) profile = p;
        state = INITIATED;
        feature.useAndroidAudio();
        feature.executor.execute(() -> {
            ImsHub.Backend b = ImsHub.backend;
            try {
                if (b == null) throw new IllegalStateException("app not ready");
                String id = b.dial(feature.subId, callee);
                callId = id;
                ImsHub.sessions.put(id, this);
                ImsCallSessionListener li = l();
                if (li != null) li.callSessionInitiating(profile);
            } catch (Throwable t) {
                state = TERMINATED;
                ImsHub.log("WARN", "系统拨号失败: " + t.getMessage());
                ImsCallSessionListener li = l();
                if (li != null) li.callSessionInitiatingFailed(
                        new ImsReasonInfo(ImsReasonInfo.CODE_LOCAL_IMS_SERVICE_DOWN, 0, String.valueOf(t.getMessage())));
            }
        });
    }

    @Override
    public void accept(int callType, ImsStreamMediaProfile media) {
        String id = callId;
        ImsHub.Backend b = ImsHub.backend;
        if (id != null && b != null) feature.executor.execute(() -> b.answer(id));
    }

    @Override
    public void reject(int reason) {
        hangup();
    }

    @Override
    public void terminate(int reason) {
        state = TERMINATING;
        hangup();
    }

    private void hangup() {
        String id = callId;
        ImsHub.Backend b = ImsHub.backend;
        if (id != null && b != null) feature.executor.execute(() -> b.hangup(id));
    }

    @Override
    public void sendDtmf(char c, Message result) {
        String id = callId;
        ImsHub.Backend b = ImsHub.backend;
        if (id != null && b != null) feature.executor.execute(() -> b.sendDtmf(id, c));
        if (result != null) result.sendToTarget();
    }

    @Override
    public void startDtmf(char c) {
        String id = callId;
        ImsHub.Backend b = ImsHub.backend;
        if (id != null && b != null) feature.executor.execute(() -> b.sendDtmf(id, c));
    }

    @Override
    public void stopDtmf() {
    }

    @Override
    public void setMute(boolean muted) {
        ImsHub.Backend b = ImsHub.backend;
        if (b != null) b.setMute(muted);
    }

    @Override
    public void hold(ImsStreamMediaProfile media) {
        ImsCallSessionListener li = l();
        if (li != null) li.callSessionHoldFailed(new ImsReasonInfo(ImsReasonInfo.CODE_UNSPECIFIED, 0, "hold not supported"));
    }

    @Override
    public void resume(ImsStreamMediaProfile media) {
        ImsCallSessionListener li = l();
        if (li != null) li.callSessionResumeFailed(new ImsReasonInfo(ImsReasonInfo.CODE_UNSPECIFIED, 0, "hold not supported"));
    }

    @Override
    public void update(int callType, ImsStreamMediaProfile media) {
        ImsCallSessionListener li = l();
        if (li != null) li.callSessionUpdateFailed(new ImsReasonInfo(ImsReasonInfo.CODE_UNSPECIFIED, 0, "not supported"));
    }

    @Override
    public void close() {
        String id = callId;
        if (id != null) ImsHub.sessions.remove(id, this);
    }

    // ------------------------------------------------------------ engine -> Telephony

    void onEngineState(String engineState, int sipCode, String reason, boolean earlyMedia) {
        ImsCallSessionListener li = l();
        switch (engineState) {
            case "ringing":
                if (!incoming && li != null) {
                    state = ESTABLISHING;
                    // INACTIVE audio = no early media: Telephony plays local ringback.
                    li.callSessionProgressing(media(earlyMedia
                            ? ImsStreamMediaProfile.DIRECTION_SEND_RECEIVE : ImsStreamMediaProfile.DIRECTION_INACTIVE));
                }
                break;
            case "active":
                if (established) break;
                established = true;
                state = ESTABLISHED;
                try {
                    ImsCallProfile p = profile;
                    if (p != null) p.getMediaProfile().copyFrom(media(ImsStreamMediaProfile.DIRECTION_SEND_RECEIVE));
                } catch (Throwable ignored) {
                }
                if (li != null) li.callSessionInitiated(profile);
                break;
            case "ended":
                int prev = state;
                state = TERMINATED;
                if (callId != null) ImsHub.sessions.remove(callId, this);
                if (li == null) break;
                ImsReasonInfo why = reasonFor(sipCode, reason, prev);
                if (!incoming && !established) li.callSessionInitiatingFailed(why);
                else li.callSessionTerminated(why);
                break;
            default:
                break;
        }
    }

    private ImsReasonInfo reasonFor(int sip, String reason, int prevState) {
        String r = reason == null ? "" : reason;
        int code;
        if (r.equals("hung up") || r.equals("cancelled") || prevState == TERMINATING) code = ImsReasonInfo.CODE_USER_TERMINATED;
        else if (r.equals("remote hung up")) code = ImsReasonInfo.CODE_USER_TERMINATED_BY_REMOTE;
        else if (r.equals("declined")) code = ImsReasonInfo.CODE_USER_DECLINE;
        else if (r.equals("missed")) code = sip == 480 ? ImsReasonInfo.CODE_USER_NOANSWER : ImsReasonInfo.CODE_USER_TERMINATED_BY_REMOTE;
        else if (sip == 486 || sip == 600) code = ImsReasonInfo.CODE_SIP_BUSY;
        else if (sip == 404 || sip == 604) code = ImsReasonInfo.CODE_SIP_NOT_FOUND;
        else if (sip == 403) code = ImsReasonInfo.CODE_SIP_FORBIDDEN;
        else if (sip == 408) code = ImsReasonInfo.CODE_SIP_REQUEST_TIMEOUT;
        else if (sip == 480) code = ImsReasonInfo.CODE_SIP_TEMPRARILY_UNAVAILABLE;
        else if (sip == 487) code = ImsReasonInfo.CODE_SIP_REQUEST_CANCELLED;
        else if (sip == 503) code = ImsReasonInfo.CODE_SIP_SERVICE_UNAVAILABLE;
        else if (sip >= 500) code = ImsReasonInfo.CODE_SIP_SERVER_ERROR;
        else if (r.startsWith("network error")) code = ImsReasonInfo.CODE_LOCAL_NETWORK_NO_SERVICE;
        else code = ImsReasonInfo.CODE_UNSPECIFIED;
        // Carrier online charging refused the call (e.g. 487 "CC_DIAM_CREDIT_LIMIT_REACHED").
        String u = r.toUpperCase(java.util.Locale.ROOT);
        if (u.contains("CREDIT_LIMIT") || u.contains("INSUFFICIENT") || u.contains("OUT_OF_CREDIT") || u.contains("NO_CREDIT")) {
            r = "余额不足（" + r + "）";
        }
        return new ImsReasonInfo(code, sip, r);
    }
}
