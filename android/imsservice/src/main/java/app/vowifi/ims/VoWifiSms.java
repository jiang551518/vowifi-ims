package app.vowifi.ims;

import android.telephony.ims.stub.ImsSmsImplBase;

import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * SMS over IMS for the system SMS stack (3GPP format). Outgoing: Android hands
 * us an SMS-SUBMIT TPDU; the engine wraps it in RP-DATA and reports the SMSC's
 * answer. Incoming: the engine hands over SMSC address + TPDU and waits; the
 * RP-ACK / RP-ERROR is sent once Android acknowledges.
 */
final class VoWifiSms extends ImsSmsImplBase {
    // ImsSmsImplBase / SmsManager values.
    private static final int SEND_STATUS_ERROR = 2, SEND_STATUS_ERROR_RETRY = 3;
    private static final int DELIVER_STATUS_OK = 1, DELIVER_STATUS_ERROR_NO_MEMORY = 3;
    private static final int RESULT_ERROR_GENERIC_FAILURE = 1, RESULT_ERROR_NO_SERVICE = 4;
    // RP causes (TS 24.011 8.2.5.4).
    private static final int RP_MEMORY_EXCEEDED = 22, RP_PROTOCOL_ERROR = 111;

    private final VoWifiMmTel feature;
    private final AtomicInteger nextToken = new AtomicInteger(1);
    /** Android token -> engine id of an incoming SMS awaiting acknowledgement. */
    private final Map<Integer, String> incoming = new ConcurrentHashMap<>();

    VoWifiSms(VoWifiMmTel feature) {
        this.feature = feature;
    }

    @Override
    public String getSmsFormat() {
        return "3gpp";
    }

    @Override
    public void onReady() {
        ImsHub.log("INFO", "系统短信通道就绪（卡槽 " + (feature.slotId + 1) + "）");
    }

    @Override
    public void sendSms(int token, int messageRef, String format, String smsc, boolean isRetry, byte[] pdu) {
        feature.executor.execute(() -> {
            ImsHub.Backend b = ImsHub.backend;
            String address;
            try {
                address = SmscAddress.fromIms(smsc);
            } catch (IllegalArgumentException e) {
                ImsHub.log("WARN", e.getMessage());
                onSendSmsResultError(token, messageRef, SEND_STATUS_ERROR, RESULT_ERROR_GENERIC_FAILURE, -1);
                return;
            }
            int cause;
            try {
                if (b == null) throw new IllegalStateException("app not ready");
                cause = b.sendSms(feature.subId, address, pdu);
            } catch (Throwable t) {
                ImsHub.log("WARN", "系统短信发送失败: " + t.getMessage());
                onSendSmsResultError(token, messageRef, SEND_STATUS_ERROR_RETRY, RESULT_ERROR_NO_SERVICE, -1);
                return;
            }
            if (cause == 0) {
                onSendSmsResultSuccess(token, messageRef);
            } else {
                ImsHub.log("WARN", "短信中心拒收（RP cause " + cause + "）");
                onSendSmsResultError(token, messageRef, SEND_STATUS_ERROR, RESULT_ERROR_GENERIC_FAILURE, cause);
            }
        });
    }

    /** Engine -> Android. */
    void deliver(String engineId, byte[] pdu, boolean statusReport) {
        int token = nextToken.getAndIncrement();
        incoming.put(token, engineId);
        try {
            if (statusReport) onSmsStatusReportReceived(token, "3gpp", pdu);
            else onSmsReceived(token, "3gpp", pdu);
        } catch (Throwable t) {
            incoming.remove(token);
            ImsHub.log("WARN", "短信交给系统失败: " + t);
            ImsHub.Backend b = ImsHub.backend;
            if (b != null) b.ackSms(engineId, false, RP_PROTOCOL_ERROR);
        }
    }

    @Override
    public void acknowledgeSms(int token, int messageRef, int result) {
        answer(token, result);
    }

    @Override
    public void acknowledgeSms(int token, int messageRef, int result, byte[] pdu) {
        answer(token, result);
    }

    @Override
    public void acknowledgeSmsReport(int token, int messageRef, int result) {
        answer(token, result);
    }

    private void answer(int token, int result) {
        String id = incoming.remove(token);
        ImsHub.Backend b = ImsHub.backend;
        if (id == null || b == null) return;
        boolean ok = result == DELIVER_STATUS_OK;
        int cause = result == DELIVER_STATUS_ERROR_NO_MEMORY ? RP_MEMORY_EXCEEDED : RP_PROTOCOL_ERROR;
        b.ackSms(id, ok, cause);
    }
}
