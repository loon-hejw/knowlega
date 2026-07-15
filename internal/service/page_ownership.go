package service

import (
	manifestfile "github.com/hejw/knowledge-core/internal/manifest"
	"github.com/hejw/knowledge-core/internal/projectlock"
)

func acquireServiceProjectLock(projectPath string) (func(), error) {
	lock, err := projectlock.Acquire(projectPath)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Release() }, nil
}

func registerPageOwnership(projectPath, pagePath, managedBy string) error {
	value, err := manifestfile.Load(projectPath)
	if err != nil {
		return err
	}
	manifestfile.RegisterPageOwner(&value, pagePath, managedBy, "")
	return manifestfile.Save(projectPath, value)
}
