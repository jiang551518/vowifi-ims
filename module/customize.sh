#!/system/bin/sh
# Installer hooks (Magisk / KernelSU / APatch).
CONF_DIR=/data/adb/vowifi
CONF=$CONF_DIR/ims.conf
mkdir -p "$CONF_DIR"
if [ ! -f "$CONF" ]; then
  cat > "$CONF" <<'CFG'
# Slots (0-based, space separated) whose system IMS (MMTEL + EMERGENCY_MMTEL)
# is taken over by the WiFi calling app after boot. Example: SLOTS="1" = SIM 2.
# Empty = no takeover.
SLOTS="1"
CFG
fi
ui_print "- 接管配置：$CONF"
ui_print "- $(grep '^SLOTS=' "$CONF")"
set_perm_recursive "$MODPATH/system/priv-app/VoWiFi" 0 0 0755 0644
set_perm "$MODPATH/system/etc/permissions/privapp-permissions-app.vowifi.xml" 0 0 0644
