//go:build !unix

package accesscheck

func freeBytes(string) (uint64, bool) { return 0, false }
