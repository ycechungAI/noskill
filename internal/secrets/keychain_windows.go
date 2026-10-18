package secrets

import (
	"errors"
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// OSKeychain uses Windows Credential Manager (advapi32 Cred*), no CGO. Untested on a real Windows
// machine so far; the calls are the documented ones.
func OSKeychain() Store { return keychain{} }

type keychain struct{}

var (
	advapi    = windows.NewLazySystemDLL("advapi32.dll")
	credWrite = advapi.NewProc("CredWriteW")
	credRead  = advapi.NewProc("CredReadW")
	credDel   = advapi.NewProc("CredDeleteW")
	credFree  = advapi.NewProc("CredFree")
)

const credTypeGeneric = 1
const credPersistLocalMachine = 2

type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

func target(name string) *uint16 {
	p, _ := syscall.UTF16PtrFromString("noskill/" + name)
	return p
}

func (keychain) Get(name string) (string, error) {
	var c *credential
	r, _, err := credRead.Call(uintptr(unsafe.Pointer(target(name))), credTypeGeneric, 0, uintptr(unsafe.Pointer(&c)))
	if r == 0 {
		if errors.Is(err, windows.ERROR_NOT_FOUND) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("credential manager: %v", err)
	}
	defer credFree.Call(uintptr(unsafe.Pointer(c)))
	blob := unsafe.Slice(c.CredentialBlob, c.CredentialBlobSize)
	u16 := make([]uint16, len(blob)/2)
	for i := range u16 {
		u16[i] = uint16(blob[2*i]) | uint16(blob[2*i+1])<<8
	}
	return string(utf16.Decode(u16)), nil
}

func (keychain) Set(name, value string) error {
	u16 := utf16.Encode([]rune(value))
	blob := make([]byte, 2*len(u16))
	for i, v := range u16 {
		blob[2*i], blob[2*i+1] = byte(v), byte(v>>8)
	}
	user, _ := syscall.UTF16PtrFromString("noskill")
	c := credential{Type: credTypeGeneric, TargetName: target(name), CredentialBlobSize: uint32(len(blob)),
		CredentialBlob: &blob[0], Persist: credPersistLocalMachine, UserName: user}
	if r, _, err := credWrite.Call(uintptr(unsafe.Pointer(&c)), 0); r == 0 {
		return fmt.Errorf("credential manager: %v", err)
	}
	return nil
}

func (keychain) Delete(name string) error {
	r, _, err := credDel.Call(uintptr(unsafe.Pointer(target(name))), credTypeGeneric, 0)
	if r == 0 {
		if errors.Is(err, windows.ERROR_NOT_FOUND) {
			return ErrNotFound
		}
		return fmt.Errorf("credential manager: %v", err)
	}
	return nil
}
