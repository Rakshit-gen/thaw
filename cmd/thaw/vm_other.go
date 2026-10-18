//go:build !linux

package main

// Booting, restoring and forking VMs needs KVM, userfaultfd and NBD, so
// those commands exist only on Linux. Everything that works on snapshots
// and traces in the store runs anywhere.
var vmCommands []command
