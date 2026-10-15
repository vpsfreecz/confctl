//go:build !linux || !cgo

package core

import "fmt"

func productionBuildLogin() (*string, error) {
	return nil, fmt.Errorf("production build login requires Linux with cgo")
}
