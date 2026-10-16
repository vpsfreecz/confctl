package core

import "github.com/vpsfreecz/confctl/internal/generation"

// generationRootDir is shared by generation writers and explicit retention.
// Each call samples login afresh; constructing an adapter does not call it.
func (e *Engine) generationRootDir() (string, error) {
	return buildRootDirectory(e.Root, productionBuildLogin)()
}

func buildRootDirectory(root string, login func() (*string, error)) func() (string, error) {
	return func() (string, error) {
		name, err := login()
		if err != nil {
			return "", err
		}
		identity, err := generation.ComputeRootIdentity(root, name)
		return identity.Directory, err
	}
}
