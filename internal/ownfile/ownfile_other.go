//go:build !linux

package ownfile

func matchDir(path string) error { return nil }
