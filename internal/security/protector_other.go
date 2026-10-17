//go:build !windows

package security

type MachineProtector struct{}

func NewMachineProtector() *MachineProtector { return &MachineProtector{} }

func (*MachineProtector) Protect([]byte) ([]byte, error) { return nil, ErrDPAPIUnavailable }

func (*MachineProtector) Unprotect([]byte) ([]byte, error) { return nil, ErrDPAPIUnavailable }
