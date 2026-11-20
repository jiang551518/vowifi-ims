package ike

import (
	"crypto/rand"
	"io"
)

func nil2rand() io.Reader { return rand.Reader }
