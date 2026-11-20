//go:build android && cgo

package mobile

/*
#cgo LDFLAGS: -landroid
#include <android/multinetwork.h>
static int vw_bind(long long handle, int fd) {
	return android_setsocknetwork((net_handle_t)handle, fd);
}
*/
import "C"

import "fmt"

// bindToNetwork pins a socket to an Android Network (Network.getNetworkHandle).
func bindToNetwork(handle int64, fd int) error {
	if r := C.vw_bind(C.longlong(handle), C.int(fd)); r != 0 {
		return fmt.Errorf("android_setsocknetwork failed (%d)", r)
	}
	return nil
}
