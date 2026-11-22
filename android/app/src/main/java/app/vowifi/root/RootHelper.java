package app.vowifi.root;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.PrintStream;
import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.util.List;

/**
 * Privileged SIM bridge. Runs as root through app_process:
 *
 *   su -c "CLASSPATH=<apk> app_process /system/bin app.vowifi.root.RootHelper"
 *
 * It speaks newline-delimited JSON on stdin/stdout. Every request carries an
 * "id" and an "op"; every response echoes the id with "ok" and either result
 * fields or "error". Only telephony binder calls are performed here; all
 * protocol logic lives in the Go core.
 *
 * Hidden telephony interfaces change between Android releases, so every call
 * goes through {@link #call} which tries several known argument layouts.
 */
public final class RootHelper {
    private static String pkg = "android";
    private static final String FEATURE = null;

    private static Object phone;      // ITelephony
    private static Object subInfo;    // IPhoneSubInfo
    private static Object isub;       // ISub
    private static Object isms;       // ISms

    public static void main(String[] args) throws Exception {
        android.util.Log.i("VoWiFi", "RootHelper start uid=" + android.os.Process.myUid() + " pid=" + android.os.Process.myPid());
        try {
            run(args);
        } catch (Throwable t) {
            android.util.Log.e("VoWiFi", "RootHelper failed", t);
            throw t;
        } finally {
            android.util.Log.i("VoWiFi", "RootHelper exit");
        }
    }

    private static void run(String[] args) throws Exception {
        PrintStream out = new PrintStream(System.out, true, "UTF-8");
        System.setOut(new PrintStream(System.err)); // keep stray prints off the protocol stream
        if (args.length > 0) pkg = args[0];
        BufferedReader in = new BufferedReader(new InputStreamReader(System.in, "UTF-8"));
        out.println(new JSONObject().put("id", 0).put("ok", true).put("ready", true).put("uid", android.os.Process.myUid()));
        String line;
        while ((line = in.readLine()) != null) {
            line = line.trim();
            if (line.isEmpty()) continue;
            JSONObject resp = new JSONObject();
            long id = 0;
            try {
                JSONObject req = new JSONObject(line);
                id = req.optLong("id");
                resp.put("id", id);
                handle(req, resp);
                resp.put("ok", true);
            } catch (Throwable t) {
                Throwable c = t;
                while (c.getCause() != null) c = c.getCause();
                resp = new JSONObject().put("id", id).put("ok", false)
                        .put("error", c.getClass().getSimpleName() + ": " + c.getMessage());
            }
            out.println(resp.toString());
        }
    }

    private static void handle(JSONObject req, JSONObject resp) throws Exception {
        String op = req.getString("op");
        switch (op) {
            case "ping":
                resp.put("uid", android.os.Process.myUid());
                break;
            case "subs":
                resp.put("subs", subscriptions());
                break;
            case "identity":
                identity(req.getInt("subId"), req.optInt("slot", -1), resp);
                break;
            case "auth": {
                // data / result are base64 as defined by TelephonyManager.getIccAuthentication
                Object r = call(subInfo(), "getIccSimChallengeResponse",
                        new Object[]{req.getInt("subId"), req.getInt("appType"), req.getInt("authType"), req.getString("data"), pkg, FEATURE},
                        new Object[]{req.getInt("subId"), req.getInt("appType"), req.getInt("authType"), req.getString("data"), pkg},
                        new Object[]{req.getInt("subId"), req.getInt("appType"), req.getInt("authType"), req.getString("data")});
                if (r == null) throw new IllegalStateException("SIM returned no authentication response");
                resp.put("result", r);
                break;
            }
            case "cards":
                resp.put("cards", cards());
                break;
            case "open":
                openChannel(req, resp);
                break;
            case "transmit":
                transmit(req, resp);
                break;
            case "close":
                closeChannel(req);
                break;
            case "methods": {
                // Debug: list methods of a telephony interface matching a substring.
                String svc = req.getString("service");
                Object target = svc.equals("phone") ? phone() : svc.equals("isub") ? isub()
                        : svc.equals("isms") ? isms() : subInfo();
                String filter = req.optString("filter", "").toLowerCase();
                JSONArray out = new JSONArray();
                for (Method m : target.getClass().getMethods()) {
                    if (!m.getName().toLowerCase().contains(filter)) continue;
                    StringBuilder sb = new StringBuilder(m.getName()).append('(');
                    for (Class<?> p : m.getParameterTypes()) sb.append(p.getSimpleName()).append(',');
                    out.put(sb.append(')').toString());
                }
                resp.put("methods", out);
                break;
            }
            case "classinfo": {
                Class<?> c = paramClass(req.getString("class"));
                JSONArray out = new JSONArray();
                for (java.lang.reflect.Constructor<?> k : c.getDeclaredConstructors()) {
                    StringBuilder sb = new StringBuilder("ctor ").append(java.lang.reflect.Modifier.toString(k.getModifiers())).append(" (");
                    for (Class<?> p : k.getParameterTypes()) sb.append(p.getSimpleName()).append(',');
                    out.put(sb.append(')').toString());
                }
                for (Field f : c.getDeclaredFields())
                    out.put("field " + java.lang.reflect.Modifier.toString(f.getModifiers()) + " " + f.getType().getSimpleName() + " " + f.getName());
                for (Method m : c.getDeclaredMethods())
                    if (java.lang.reflect.Modifier.isPublic(m.getModifiers())) out.put("method " + m.getName() + "/" + m.getParameterCount());
                out.put("name " + c.getName());
                resp.put("methods", out);
                break;
            }
            case "netsel": {
                // Per-SIM "no signal": manual selection of the test PLMN
                // 001-01 (not persisted, so a reboot reverts it). The radio
                // and the SIM stay on, so the other SIM and AKA are untouched.
                // Telephony blocks while the modem searches (can exceed 20 s),
                // so run it in order on a background thread and reply now.
                final int sub = req.getInt("subId");
                final boolean block = req.getBoolean("block");
                NETSEL.execute(() -> {
                    try {
                        if (block) {
                            Class<?> oi = Class.forName("com.android.internal.telephony.OperatorInfo");
                            Object info = oi.getConstructor(String.class, String.class, String.class)
                                    .newInstance("WiFi Calling", "WiFi", "00101");
                            call(phone(), "setNetworkSelectionModeManual", new Object[]{sub, info, false});
                        } else {
                            call(phone(), "setNetworkSelectionModeAutomatic", new Object[]{sub});
                        }
                    } catch (Throwable t) {
                        System.err.println("netsel failed: " + t);
                    }
                });
                resp.put("result", "queued");
                break;
            }
            case "nettypes": {
                // Allowed network types of one SIM for one reason
                // (TelephonyManager.ALLOWED_NETWORK_TYPES_REASON_*). The modem
                // uses the intersection of all reasons. With "mask" it sets,
                // and always returns the value now in effect for that reason.
                int sub = req.getInt("subId");
                int reason = req.getInt("reason");
                if (req.has("mask")) {
                    call(phone(), "setAllowedNetworkTypesForReason", new Object[]{sub, reason, req.getLong("mask")});
                }
                Object r = call(phone(), "getAllowedNetworkTypesForReason", new Object[]{sub, reason});
                resp.put("result", String.valueOf(r));
                break;
            }
            case "svcstate": {
                // "voice,data,manual,dataRat" registration states of a slot's ServiceState
                // (0 in service, 1 out of service, 2 emergency only, 3 power off).
                Object ss = call(phone(), "getServiceStateForSlot",
                        new Object[]{req.getInt("slot"), true, true, "android", null});
                Object v = ss.getClass().getMethod("getState").invoke(ss);
                Object d = ss.getClass().getMethod("getDataRegistrationState").invoke(ss);
                Object m = ss.getClass().getMethod("getIsManualSelection").invoke(ss);
                Object rat = -1;
                try { rat = ss.getClass().getMethod("getRilDataRadioTechnology").invoke(ss); } catch (Throwable ignored) { }
                resp.put("result", v + "," + d + "," + m + "," + rat);
                break;
            }
            case "cellinfo": {
                // Read-only cellular snapshot of one SIM for the WebUI: radio
                // technology, band / channel, channel bandwidths and signal.
                // Location access is not renounced: the cell identity (band,
                // channel) is location data and would be stripped otherwise.
                Object ss = call(phone(), "getServiceStateForSlot",
                        new Object[]{req.getInt("slot"), false, false, "android", null});
                JSONObject o = new JSONObject();
                for (Object nri : (java.util.List<?>) ss.getClass().getMethod("getNetworkRegistrationInfoList").invoke(ss)) {
                    // Cellular (WWAN) registration; prefer the data (PS) domain.
                    if ((int) nri.getClass().getMethod("getTransportType").invoke(nri) != 1) continue;
                    int domain = (int) nri.getClass().getMethod("getDomain").invoke(nri);
                    if (o.has("rat") && (domain & 2) == 0) continue;
                    o.put("rat", nri.getClass().getMethod("getAccessNetworkTechnology").invoke(nri));
                    o.put("reg", nri.getClass().getMethod("getNetworkRegistrationState").invoke(nri));
                    Object ci = nri.getClass().getMethod("getCellIdentity").invoke(nri);
                    if (ci != null) {
                        o.put("cell", ci.getClass().getSimpleName());
                        for (String m : new String[]{"getBands", "getEarfcn", "getNrarfcn", "getUarfcn", "getArfcn", "getPci"}) {
                            try {
                                Object v = ci.getClass().getMethod(m).invoke(ci);
                                if (v instanceof int[]) { JSONArray a = new JSONArray(); for (int b : (int[]) v) a.put(b); v = a; }
                                o.put(m.substring(3).toLowerCase(java.util.Locale.ROOT), v);
                            } catch (NoSuchMethodException ignored) { }
                        }
                    }
                }
                try {
                    int[] bw = (int[]) ss.getClass().getMethod("getCellBandwidths").invoke(ss);
                    JSONArray a = new JSONArray(); for (int b : bw) a.put(b); o.put("bandwidths", a);
                } catch (Throwable ignored) { }
                try { o.put("nrState", ss.getClass().getMethod("getNrState").invoke(ss)); } catch (Throwable ignored) { }
                try { o.put("roaming", ss.getClass().getMethod("getRoaming").invoke(ss)); } catch (Throwable ignored) { }
                try {
                    Object sig = call(phone(), "getSignalStrength", new Object[]{req.getInt("subId")});
                    for (Object c : (java.util.List<?>) sig.getClass().getMethod("getCellSignalStrengths").invoke(sig)) {
                        JSONObject s = new JSONObject();
                        s.put("type", c.getClass().getSimpleName());
                        s.put("dbm", c.getClass().getMethod("getDbm").invoke(c));
                        s.put("level", c.getClass().getMethod("getLevel").invoke(c));
                        for (String m : new String[]{"getRsrp", "getRsrq", "getRssnr", "getSsRsrp", "getSsSinr"}) {
                            try { s.put(m.substring(3).toLowerCase(java.util.Locale.ROOT), c.getClass().getMethod(m).invoke(c)); }
                            catch (NoSuchMethodException ignored) { }
                        }
                        o.append("signal", s);
                    }
                } catch (Throwable ignored) { }
                resp.put("result", o.toString());
                break;
            }
            case "netselmode": {
                // 1 = automatic, 2 = manual, 0 = unknown.
                Object r = call(phone(), "getNetworkSelectionMode", new Object[]{req.getInt("subId")});
                resp.put("result", String.valueOf(r));
                break;
            }
            case "voiceoverride": {
                // Telecom's "voice service available over non-cellular" signal:
                // GsmCdmaPhone then reports voice IN_SERVICE (as with native
                // WiFi calling) so outgoing calls reach the IMS service.
                call(phone(), "setVoiceServiceStateOverride",
                        new Object[]{req.getInt("subId"), req.getBoolean("on"), "android"});
                break;
            }
            case "brand": {
                // Operator brand override (shown as the SPN). A SIM without its own
                // SPN (e.g. 3UK) otherwise has an empty carrier name over WiFi
                // calling when it has no cellular registration. Empty clears it.
                String b = req.optString("brand", "");
                Object r = call(phone(), "setOperatorBrandOverride", new Object[]{req.getInt("subId"), b.isEmpty() ? null : b});
                resp.put("result", String.valueOf(r));
                break;
            }
            case "crosssim": {
                // Per-SIM "cross-SIM calling" user setting. ColorOS only keeps the
                // WiFi calling status bar icon without WiFi when the registration
                // is cross-SIM and this setting is on. Returns the previous value.
                Object before = call(phone(), "isCrossSimCallingEnabledByUser", new Object[]{req.getInt("subId")});
                if (req.has("on")) call(phone(), "setCrossSimCallingEnabled", new Object[]{req.getInt("subId"), req.getBoolean("on")});
                resp.put("result", String.valueOf(before));
                break;
            }
            case "wfc": {
                // The per-SIM system "WLAN calling" switch (Settings > SIM). It is the
                // module's master switch: on = take the slot over and connect, off =
                // disconnect and hand the slot back. Returns the value before "on".
                Object before = call(phone(), "isVoWiFiSettingEnabled", new Object[]{req.getInt("subId")});
                if (req.has("on")) call(phone(), "setVoWiFiSettingEnabled", new Object[]{req.getInt("subId"), req.getBoolean("on")});
                resp.put("result", String.valueOf(before));
                break;
            }
            case "invoke": {
                // Debug: call an arbitrary method on a telephony interface.
                String svc = req.getString("service");
                Object target = svc.equals("phone") ? phone() : svc.equals("isub") ? isub()
                        : svc.equals("isms") ? isms() : subInfo();
                JSONArray a = req.getJSONArray("args");
                Object[] args = new Object[a.length()];
                for (int i = 0; i < args.length; i++) args[i] = a.isNull(i) ? null : a.get(i);
                Object r = call(target, req.getString("method"), args);
                resp.put("result", r == null ? JSONObject.NULL : String.valueOf(r));
                break;
            }
            default:
                throw new IllegalArgumentException("unknown op " + op);
        }
    }

    // ---------------------------------------------------------------- services

    private static Object service(String name, String stub) throws Exception {
        Object binder = Class.forName("android.os.ServiceManager").getMethod("getService", String.class).invoke(null, name);
        if (binder == null) throw new IllegalStateException("service " + name + " unavailable");
        Class<?> c = Class.forName(stub);
        return c.getMethod("asInterface", android.os.IBinder.class).invoke(null, binder);
    }

    private static Object phone() throws Exception {
        if (phone == null) phone = service("phone", "com.android.internal.telephony.ITelephony$Stub");
        return phone;
    }

    private static Object subInfo() throws Exception {
        if (subInfo == null) subInfo = service("iphonesubinfo", "com.android.internal.telephony.IPhoneSubInfo$Stub");
        return subInfo;
    }

    private static Object isub() throws Exception {
        if (isub == null) isub = service("isub", "com.android.internal.telephony.ISub$Stub");
        return isub;
    }

    private static Object isms() throws Exception {
        if (isms == null) isms = service("isms", "com.android.internal.telephony.ISms$Stub");
        return isms;
    }

    /** Invoke the first overload whose arity and parameter types fit one of the candidate argument lists. */
    private static final java.util.concurrent.ExecutorService NETSEL =
            java.util.concurrent.Executors.newSingleThreadExecutor();

    private static Object call(Object target, String name, Object[]... candidates) throws Exception {
        Exception last = null;
        for (Object[] args : candidates) {
            for (Method m : target.getClass().getMethods()) {
                if (!m.getName().equals(name) || m.getParameterTypes().length != args.length) continue;
                if (!fits(m.getParameterTypes(), args)) continue;
                try {
                    return m.invoke(target, args);
                } catch (java.lang.reflect.InvocationTargetException e) {
                    Throwable cause = e.getCause();
                    if (cause instanceof Exception) last = (Exception) cause; else throw e;
                }
            }
        }
        if (last != null) throw last;
        throw new NoSuchMethodException(target.getClass().getName() + "." + name);
    }

    private static boolean fits(Class<?>[] types, Object[] args) {
        for (int i = 0; i < types.length; i++) {
            Class<?> t = types[i];
            Object a = args[i];
            if (a == null) {
                if (t.isPrimitive()) return false;
                continue;
            }
            if (t == int.class) { if (!(a instanceof Integer)) return false; continue; }
            if (t == long.class) { if (!(a instanceof Long)) return false; continue; }
            if (t == boolean.class) { if (!(a instanceof Boolean)) return false; continue; }
            if (!t.isInstance(a)) return false;
        }
        return true;
    }

    private static Object quiet(Object target, String name, Object[]... candidates) {
        try {
            return call(target, name, candidates);
        } catch (Throwable t) {
            return null;
        }
    }

    // ----------------------------------------------------------- subscriptions

    private static JSONArray subscriptions() throws Exception {
        Object list = call(isub(), "getActiveSubscriptionInfoList",
                new Object[]{pkg, FEATURE, true},
                new Object[]{pkg, FEATURE},
                new Object[]{pkg});
        JSONArray arr = new JSONArray();
        if (list == null) return arr;
        for (Object info : (List<?>) list) {
            JSONObject o = new JSONObject();
            o.put("subId", get(info, "getSubscriptionId"));
            o.put("slot", get(info, "getSimSlotIndex"));
            o.put("mcc", get(info, "getMccString"));
            o.put("mnc", get(info, "getMncString"));
            o.put("carrierName", String.valueOf(get(info, "getCarrierName")));
            o.put("displayName", String.valueOf(get(info, "getDisplayName")));
            o.put("iccid", get(info, "getIccId"));
            o.put("countryIso", get(info, "getCountryIso"));
            o.put("embedded", get(info, "isEmbedded"));
            o.put("cardId", get(info, "getCardId"));
            o.put("portIndex", get(info, "getPortIndex"));
            o.put("carrierId", get(info, "getCarrierId"));
            arr.put(o);
        }
        return arr;
    }

    private static Object get(Object o, String getter) {
        try {
            Object v = o.getClass().getMethod(getter).invoke(o);
            return v == null ? JSONObject.NULL : v;
        } catch (Throwable t) {
            return JSONObject.NULL;
        }
    }

    private static void identity(int subId, int slot, JSONObject resp) throws Exception {
        Object si = subInfo();
        resp.put("imsi", call(si, "getSubscriberIdForSubscriber",
                new Object[]{subId, pkg, FEATURE}, new Object[]{subId, pkg}));
        resp.put("iccid", nul(quiet(si, "getIccSerialNumberForSubscriber",
                new Object[]{subId, pkg, FEATURE}, new Object[]{subId, pkg})));
        resp.put("msisdn", nul(quiet(si, "getLine1NumberForSubscriber",
                new Object[]{subId, pkg, FEATURE}, new Object[]{subId, pkg})));
        resp.put("gid1", nul(quiet(si, "getGroupIdLevel1ForSubscriber",
                new Object[]{subId, pkg, FEATURE}, new Object[]{subId, pkg})));
        resp.put("impi", nul(quiet(si, "getIsimImpi", new Object[]{subId})));
        resp.put("domain", nul(quiet(si, "getIsimDomain", new Object[]{subId})));
        Object impu = quiet(si, "getIsimImpu", new Object[]{subId});
        JSONArray impus = new JSONArray();
        if (impu instanceof String[]) for (String s : (String[]) impu) impus.put(s);
        resp.put("impu", impus);
        resp.put("smsc", nul(quiet(isms(), "getSmscAddressFromIccEfForSubscriber",
                new Object[]{subId, pkg}, new Object[]{subId, pkg, FEATURE})));
        if (slot >= 0) {
            resp.put("imei", nul(quiet(phone(), "getImeiForSlot",
                    new Object[]{slot, pkg, FEATURE}, new Object[]{slot, pkg})));
            resp.put("svn", nul(quiet(si, "getDeviceSvnUsingSubId",
                    new Object[]{subId, pkg, FEATURE}, new Object[]{subId, pkg})));
        }
    }

    private static Object nul(Object v) {
        return v == null ? JSONObject.NULL : v;
    }

    // ------------------------------------------------------------------- cards

    private static JSONArray cards() throws Exception {
        JSONArray arr = new JSONArray();
        Object list = call(phone(), "getUiccCardsInfo", new Object[]{pkg});
        if (list == null) return arr;
        for (Object info : (List<?>) list) {
            JSONObject o = new JSONObject();
            o.put("cardId", get(info, "getCardId"));
            o.put("eid", get(info, "getEid"));
            o.put("iccid", get(info, "getIccId"));
            o.put("physicalSlot", get(info, "getPhysicalSlotIndex"));
            o.put("euicc", get(info, "isEuicc"));
            o.put("removable", get(info, "isRemovable"));
            arr.put(o);
        }
        return arr;
    }

    /**
     * Resolve a class by name, falling back to the parameter types of the
     * telephony interfaces (classes such as IccLogicalChannelRequest are not
     * visible to the app_process boot class loader by name).
     */
    private static Class<?> paramClass(String name) throws Exception {
        try {
            return Class.forName(name);
        } catch (ClassNotFoundException e) {
            for (Object t : new Object[]{phone(), subInfo(), isub(), isms()}) {
                for (Method m : t.getClass().getMethods()) {
                    for (Class<?> p : m.getParameterTypes())
                        if (p.getName().equals(name) || p.getSimpleName().equals(name.substring(name.lastIndexOf('.') + 1))) return p;
                }
            }
            throw e;
        }
    }

    private static Object channelRequest(JSONObject req, String aid) throws Exception {
        Class<?> c = paramClass("android.telephony.IccLogicalChannelRequest");
        java.lang.reflect.Constructor<?> k = c.getDeclaredConstructor();
        k.setAccessible(true);
        Object r = k.newInstance();
        setField(r, "subId", req.optInt("subId", -1));
        setField(r, "slotIndex", req.optInt("slot", -1));
        setField(r, "portIndex", req.optInt("port", 0));
        setField(r, "callingPackage", pkg);
        if (aid != null) setField(r, "aid", aid);
        setField(r, "p2", req.optInt("p2", 0));
        setField(r, "channel", req.optInt("channel", -1));
        setField(r, "binder", new android.os.Binder());
        return r;
    }

    private static void setField(Object o, String name, Object value) {
        try {
            Field f = o.getClass().getField(name);
            f.set(o, value);
        } catch (Throwable ignored) {
        }
    }

    private static void openChannel(JSONObject req, JSONObject resp) throws Exception {
        String aid = req.getString("aid");
        Object r;
        try {
            r = call(phone(), "iccOpenLogicalChannel", new Object[]{channelRequest(req, aid)});
        } catch (ClassNotFoundException | NoSuchMethodException e) {
            r = call(phone(), "iccOpenLogicalChannel",
                    new Object[]{req.getInt("subId"), pkg, aid, req.optInt("p2", 0)});
        }
        int channel = (Integer) r.getClass().getMethod("getChannel").invoke(r);
        int status = (Integer) r.getClass().getMethod("getStatus").invoke(r);
        byte[] select = (byte[]) r.getClass().getMethod("getSelectResponse").invoke(r);
        resp.put("channel", channel);
        resp.put("status", status);
        resp.put("select", select == null ? "" : hex(select));
        if (channel < 0 || status != 1) throw new IllegalStateException("open logical channel failed, status=" + status);
    }

    private static void transmit(JSONObject req, JSONObject resp) throws Exception {
        int channel = req.getInt("channel");
        int cla = req.getInt("cla"), ins = req.getInt("ins"), p1 = req.getInt("p1"), p2 = req.getInt("p2"), p3 = req.getInt("p3");
        String data = req.optString("data", "");
        Object r;
        if (req.optInt("slot", -1) >= 0 && req.optInt("subId", -1) < 0) {
            r = call(phone(), "iccTransmitApduLogicalChannelByPort",
                    new Object[]{req.getInt("slot"), req.optInt("port", 0), channel, cla, ins, p1, p2, p3, data});
        } else {
            r = call(phone(), "iccTransmitApduLogicalChannel",
                    new Object[]{req.getInt("subId"), channel, cla, ins, p1, p2, p3, data});
        }
        resp.put("response", r == null ? "" : r);
    }

    private static void closeChannel(JSONObject req) throws Exception {
        try {
            call(phone(), "iccCloseLogicalChannel", new Object[]{channelRequest(req, null)});
        } catch (ClassNotFoundException | NoSuchMethodException e) {
            call(phone(), "iccCloseLogicalChannel", new Object[]{req.getInt("subId"), req.getInt("channel")});
        }
    }

    private static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder(b.length * 2);
        for (byte x : b) sb.append(String.format("%02X", x & 0xff));
        return sb.toString();
    }
}
