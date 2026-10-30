//go:build windows

// Windows 凭证加密：DPAPI（CryptProtectData/CryptUnprotectData），
// 当前用户上下文，纯 syscall 无 cgo（保持交叉编译能力）。
package credential

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	modcrypt32             = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = modcrypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = modcrypt32.NewProc("CryptUnprotectData")
	modkernel32            = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree          = modkernel32.NewProc("LocalFree")
)

// CRYPTPROTECT_UI_FORBIDDEN：密文操作禁止弹出任何 UI（服务/计划任务
// 上下文也不挂起）。
const cryptProtectUIForbidden = 0x1

// dataBlob 对应 Win32 CRYPT_INTEGER_BLOB。
type dataBlob struct {
	cbData uint32
	pbData *byte
}

// seal 用 DPAPI 加密明文。密文绑定当前 Windows 用户，跨用户/跨机器
// 无法解开（设计预期：凭证仅本机本用户可用，F10）。
func seal(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("dpapi seal: 空明文")
	}
	in := dataBlob{cbData: uint32(len(plain)), pbData: &plain[0]}
	var out dataBlob
	desc, err := syscall.UTF16PtrFromString("yardmaster-credentials")
	if err != nil {
		return nil, fmt.Errorf("dpapi seal: %w", err)
	}
	r1, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		uintptr(unsafe.Pointer(desc)),
		0, // pOptionalEntropy
		0, // pvReservedAuth
		0, // pvReserved
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("CryptProtectData: %w", callErr)
	}
	defer func() { _, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData))) }()
	blob := unsafe.Slice(out.pbData, out.cbData)
	cp := make([]byte, len(blob))
	copy(cp, blob)
	return cp, nil
}

// unseal 用 DPAPI 解密密文。解不开（他用户密文/损坏）返回错误，
// 由调用方按 ErrUnreadable fail-closed。
func unseal(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("dpapi unseal: 空密文")
	}
	in := dataBlob{cbData: uint32(len(blob)), pbData: &blob[0]}
	var out dataBlob
	r1, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // ppszDataDescr（不取）
		0, // pOptionalEntropy
		0, // pvReservedAuth
		0, // pvReserved
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", callErr)
	}
	defer func() { _, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData))) }()
	plain := unsafe.Slice(out.pbData, out.cbData)
	cp := make([]byte, len(plain))
	copy(cp, plain)
	return cp, nil
}
