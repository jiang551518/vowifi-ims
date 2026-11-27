package app.vowifi.ims;

import android.telephony.PhoneNumberUtils;

final class SmscAddress {
    private SmscAddress() {}

    // Android IMS supplies the length-prefixed SMSC as hex. Empty means use the SIM default.
    static String fromIms(String encoded) {
        if (encoded == null || encoded.isEmpty() || encoded.equals("00")) return "";
        if ((encoded.length() & 1) != 0 || encoded.length() < 6 || encoded.length() > 512) {
            throw new IllegalArgumentException("Invalid SMSC length");
        }
        byte[] bytes = new byte[encoded.length() / 2];
        for (int i = 0; i < bytes.length; i++) {
            int hi = Character.digit(encoded.charAt(i * 2), 16);
            int lo = Character.digit(encoded.charAt(i * 2 + 1), 16);
            if (hi < 0 || lo < 0) throw new IllegalArgumentException("Invalid SMSC hex");
            bytes[i] = (byte) ((hi << 4) | lo);
        }
        if ((bytes[0] & 255) != bytes.length - 1) {
            throw new IllegalArgumentException("Invalid SMSC length prefix");
        }
        for (int i = 2; i < bytes.length; i++) {
            int octet = bytes[i] & 0xff;
            if ((octet & 15) == 15 || ((octet >>> 4) == 15 && i != bytes.length - 1)) {
                throw new IllegalArgumentException("Invalid SMSC padding");
            }
        }
        return PhoneNumberUtils.calledPartyBCDToString(bytes, 1, bytes.length - 1);
    }
}
