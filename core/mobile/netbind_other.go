//go:build !(android && cgo)

package mobile

import "errors"

func bindToNetwork(handle int64, fd int) error {
	return errors.New("network binding requires the Android build")
}
