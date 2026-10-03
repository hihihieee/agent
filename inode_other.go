//go:build !linux && !darwin && !freebsd

package main

import "os"

func inode(_ os.FileInfo) uint64 { return 0 }
