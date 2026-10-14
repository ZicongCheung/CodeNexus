//go:build windows

package crypto

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// dpapiKeeper protects the KEK with the Windows Data Protection API, which
// binds the ciphertext to the current user account.
type dpapiKeeper struct{ path string }

func newDPAPIKeeper(dir string) Keeper {
	if dir == "" {
		return nil
	}
	return &dpapiKeeper{path: filepath.Join(dir, "master.dpapi")}
}

func (k *dpapiKeeper) Name() string { return "dpapi" }

func (k *dpapiKeeper) Key() ([]byte, error) {
	if b, err := os.ReadFile(k.path); err == nil {
		return dpapiUnprotect(b)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key, err := randomBytes(KeySize)
	if err != nil {
		return nil, err
	}
	blob, err := dpapiProtect(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(k.path), 0o700); err != nil {
		return nil, err
	}
	if err := writeAtomicFile(k.path, blob); err != nil {
		return nil, err
	}
	return key, nil
}

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procProtect        = crypt32.NewProc("CryptProtectData")
	procUnprotect      = crypt32.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32.NewProc("LocalFree")
	cryptProtectUIForb = uintptr(0x1) // CRYPTPROTECT_UI_FORBIDDEN
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func dpapiProtect(data []byte) ([]byte, error) {
	return dpapiCall(procProtect, data)
}

func dpapiUnprotect(data []byte) ([]byte, error) {
	return dpapiCall(procUnprotect, data)
}

func dpapiCall(proc *syscall.LazyProc, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, ErrFormat
	}
	in := dataBlob{cbData: uint32(len(data)), pbData: &data[0]}
	var out dataBlob
	r, _, err := proc.Call(
		uintptr(unsafe.Pointer(&in)),  // pDataIn
		0,                             // szDataDescr / ppszDataDescr
		0,                             // pOptionalEntropy
		0,                             // pvReserved
		0,                             // pPromptStruct
		cryptProtectUIForb,            // dwFlags
		uintptr(unsafe.Pointer(&out)), // pDataOut
	)
	runtime.KeepAlive(data)
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	outCopy := make([]byte, out.cbData)
	copy(outCopy, unsafe.Slice(out.pbData, out.cbData))
	return outCopy, nil
}
