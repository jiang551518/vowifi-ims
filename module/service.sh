#!/system/bin/sh
# Late start. The app (a persistent system app) decides on its own which SIM
# slot's IMS it serves: the chosen SIM's system "WLAN calling" switch on = take
# the slot over and connect, off = hand it back to the phone's own IMS. Here we
# only make sure the app process is up once boot has completed. The takeover is
# never persistent, so without the app every slot stays with the phone's IMS.
until [ "$(getprop sys.boot_completed)" = "1" ]; do sleep 3; done
sleep 20
# ColorOS's app-clone user (and any other secondary user) also gets a copy of
# this persistent system app; it is not needed there.
for u in $(pm list users 2>/dev/null | sed -n 's/.*UserInfo{\([0-9]*\):.*/\1/p'); do
  [ "$u" = 0 ] || pm uninstall -k --user "$u" app.vowifi >/dev/null 2>&1
done
am broadcast -n app.vowifi/.control.ControlReceiver -a app.vowifi.CONTROL --es cmd refresh >/dev/null 2>&1
