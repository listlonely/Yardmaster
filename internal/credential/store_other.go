//go:build !windows

// 非 Windows 平台：凭证库以 0600 明文 JSON 落盘（依赖文件系统权限；
// WriteAtomic 默认 0600）。seal/unseal 为直通。
package credential

import "fmt"

func seal(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("seal: 空明文")
	}
	return plain, nil
}

func unseal(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("unseal: 空内容")
	}
	return plain, nil
}
