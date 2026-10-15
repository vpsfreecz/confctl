//go:build linux && cgo

package core

/*
#include "build_login_libc.h"
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

var buildLoginMu sync.Mutex

// productionBuildLogin copies libc's login (or C USER only on null) at the
// actual root demand. No static C pointer escapes or crosses another owned
// lookup. The caller must keep the process environment stable during lookup.
func productionBuildLogin() (*string, error) {
	buildLoginMu.Lock()
	defer buildLoginMu.Unlock()
	var value *C.char
	switch C.confctl_login_copy(&value) {
	case 0:
		return nil, nil
	case 1:
		defer C.free(unsafe.Pointer(value))
		name := C.GoString(value)
		return &name, nil
	default:
		return nil, fmt.Errorf("build login allocation failed")
	}
}
